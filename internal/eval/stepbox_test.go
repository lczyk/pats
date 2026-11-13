package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// localBox is a step sandbox whose binds are the host's own paths, as a
// bind-mount driver's are: its execs run on this machine with the real sh,
// and only the box's own paths (the script dir, HOME) live under a root of
// the test's. what a step writes through a bind is the host's at once.
type localBox struct {
	t     *testing.T
	root  string
	mu    sync.Mutex
	specs []sandbox.Spec
	execs []sandbox.Cmd
}

func (b *localBox) Start(_ context.Context, spec sandbox.Spec) (sandbox.Session, error) {
	b.mu.Lock()
	b.specs = append(b.specs, spec)
	b.mu.Unlock()
	return &localSess{b: b, spec: spec}, nil
}

func (b *localBox) Run(context.Context, sandbox.Spec, io.Writer, io.Writer) (int, error) {
	return 0, errors.New("not here")
}

// in maps a path of the box's own onto the test's root.
func (b *localBox) in(p string) string {
	if strings.HasPrefix(p, "/tmp/pats-step") {
		return filepath.Join(b.root, p)
	}
	return p
}

type localSess struct {
	b    *localBox
	spec sandbox.Spec
}

func (s *localSess) Push(_ context.Context, hostPath, path string) error {
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	fi, err := os.Stat(hostPath)
	if err != nil {
		return err
	}
	dst := s.b.in(path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, fi.Mode())
}

func (s *localSess) Pull(context.Context, string, string) error { return nil }
func (s *localSess) HandOver(context.Context, ...string) error  { return nil }
func (s *localSess) Sync(context.Context) error                 { return nil }
func (s *localSess) Close() error                               { return nil }

func (s *localSess) Exec(ctx context.Context, c sandbox.Cmd, stdout, stderr io.Writer) (int, error) {
	// a bind-mount driver binds the workdir on every exec (bwrap does).
	if _, err := os.Stat(s.spec.Workdir); err != nil {
		return -1, fmt.Errorf("the box's workdir is gone: %w", err)
	}
	s.b.mu.Lock()
	s.b.execs = append(s.b.execs, c)
	s.b.mu.Unlock()
	var argv []string
	for _, a := range c.Argv {
		argv = append(argv, s.b.in(a))
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+s.b.in(v))
	}
	cmd.Dir = s.b.in(c.Dir)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// withStepBox swaps in a localBox for the step sandbox (image "tools") and
// the run's fake for the agent's, and keeps the leftover kill, which on the
// host would reach every process of the user, from ever being sent.
func withStepBox(t *testing.T) (*localBox, *fakeSandbox) {
	box := &localBox{t: t, root: t.TempDir()}
	agent := &fakeSandbox{}
	oldNew, oldKill, oldRemap := newSandbox, killsLeftovers, remapCheck
	newSandbox = func(o sandbox.Options, s sandbox.SandboxOptions) (sandbox.Sandbox, error) {
		if s.Image == "tools" {
			return box, nil
		}
		return agent, nil
	}
	killsLeftovers = func(string) bool { return false }
	remapCheck = func(context.Context, config.Sandbox) (bool, error) { return false, nil }
	t.Cleanup(func() { newSandbox, killsLeftovers, remapCheck = oldNew, oldKill, oldRemap })
	return box, agent
}

// a task's steps run in its step sandbox: stage and prompt-cmd in a box
// before the agent with the workdir, the output dir and the agent's home
// writable, collect in another after it; their logs land in the output
// dir, prompt-cmd's stdout is the prompt, and the metadata names the box.
func TestRunPairStepSandbox(t *testing.T) {
	box, agent := withStepBox(t)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "helper"), []byte("#!/bin/sh\necho helped\n"), 0o755))
	cfg := parseCfg(t, `
sbs: {kind: sandbox, items: {sxx: {image: s}, too: {image: tools, env: {FROM_SANDBOX: sb}}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {step-sandbox: too, step-env: [STEP_TOKEN], env: {TASK_VAR: tv},
  stage: 'helper > "$PATS_WORKDIR/staged"; echo "$HOME $FROM_SANDBOX $STEP_TOKEN $TASK_VAR"',
  prompt-cmd: 'printf "do it"',
  collect: 'cp "$PATS_WORKDIR/staged" "$PATS_OUTPUT_DIR/collected"'}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	t.Setenv("STEP_TOKEN", "tok")
	var prompt string
	agent.onRun = func(spec sandbox.Spec) {
		for _, m := range spec.Mounts {
			if m.Container == sandbox.HomeMount {
				b, _ := os.ReadFile(filepath.Join(m.Host, promptName))
				prompt = string(b)
			}
		}
	}
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err, out.String())
	pair := filepath.Join(runDir, "axx", "txx")

	collected, err := os.ReadFile(filepath.Join(pair, "collected"))
	require.NoError(t, err, out.String())
	assert.Equal(t, string(collected), "helped\n")
	stageLog, err := os.ReadFile(filepath.Join(pair, "stage.log"))
	require.NoError(t, err)
	// HOME is the box's own (the fake keeps it under the test's root).
	assert.That(t, strings.HasSuffix(strings.TrimSpace(string(stageLog)), stepHome+" sb tok tv"), "stage saw: %s", stageLog)
	_, err = os.Stat(filepath.Join(pair, "collect.log"))
	assert.NoError(t, err)
	assert.Equal(t, prompt, "do it")

	var meta pairMeta
	b, err := os.ReadFile(filepath.Join(pair, "metadata.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.NotNil(t, meta.Steps)
	assert.Equal(t, meta.Steps.Sandbox, "too")

	// preflight, before, after: the tree read-only with the runs dir masked
	// in every one, the phase's own dirs writable.
	require.Len(t, box.specs, 3)
	for _, sp := range box.specs {
		tree := sp.Mounts[0]
		assert.Equal(t, tree.Host, dir)
		assert.That(t, tree.ReadOnly, "the tree is writable")
		assert.ContainsString(t, strings.Join(tree.Masks, " "), filepath.Join(dir, ".pats"))
	}
	writable := func(sp sandbox.Spec) int {
		n := 0
		for _, m := range sp.Mounts {
			if !m.ReadOnly {
				n++
			}
		}
		return n
	}
	assert.Equal(t, writable(box.specs[0]), 0)
	assert.Equal(t, writable(box.specs[1]), 3)
	assert.Equal(t, writable(box.specs[2]), 2)
}

// a step that fails in the box is the pair's setup error, named as stage.
func TestRunPairStepSandboxStageFails(t *testing.T) {
	withStepBox(t)
	dir := t.TempDir()
	cfg := parseCfg(t, `
sbs: {kind: sandbox, items: {sxx: {image: s}, too: {image: tools}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {prompt: p, step-sandbox: too, stage: 'echo broke >&2; exit 3'}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, _ := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	var meta pairMeta
	b, err := os.ReadFile(filepath.Join(runDir, "axx", "txx", "metadata.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	assert.Equal(t, meta.Status, "error")
	assert.ContainsString(t, meta.Error, "stage: exit status 3")
}

// a scorer's steps run in its step sandbox: one box serves every pair of
// the run one at a time, with the run dir read-only.
func TestScoreStepSandbox(t *testing.T) {
	box, _ := withStepBox(t)
	dir := t.TempDir()
	cfg := parseCfg(t, `
sbs: {kind: sandbox, items: {sxx: {image: s}, too: {image: tools}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
tks: {kind: task, items: {txx: {prompt: p}, tyy: {prompt: p}}}
scx: {kind: scorer, items: {scx: {score: 'test -d "$PATS_SCORED_DIR" && echo 1', step-sandbox: too}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: tks, scorers: scx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	require.NoError(t, err, out.String())
	box.specs = nil
	rep, err := Score(cfg, ScoreOptions{ConfigDir: dir, RunDir: runDir, Out: &out, Jobs: 1, noReport: true})
	require.NoError(t, err, out.String())
	require.Len(t, rep.Scores, 2)
	for _, s := range rep.Scores {
		assert.Equal(t, s.Score, 1.0)
	}
	assert.Equal(t, rep.Steps["scx"].Sandbox, "too")
	require.Len(t, box.specs, 1)
	ro := false
	for _, m := range box.specs[0].Mounts {
		if m.Host == runDir {
			ro = m.ReadOnly
		}
	}
	assert.That(t, ro, "the run dir is not bound read-only")
}

// the tree is the work tree that holds the root, never the home or above.
func TestStepTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SNAP_REAL_HOME", "")
	repo := filepath.Join(home, "repo")
	root := filepath.Join(repo, "tests", "skills")
	require.NoError(t, os.MkdirAll(root, 0o755))
	got, err := stepTree(root)
	require.NoError(t, err)
	assert.Equal(t, got, root, "no .git: the root alone")

	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	got, err = stepTree(root)
	require.NoError(t, err)
	assert.Equal(t, got, repo)

	// a dotfiles repo at the home is not taken.
	require.NoError(t, os.RemoveAll(filepath.Join(repo, ".git")))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".git"), 0o755))
	got, err = stepTree(root)
	require.NoError(t, err)
	assert.Equal(t, got, root)

	_, err = stepTree(home)
	assert.Error(t, err, "is the home or above it")
}

// a prompt-cmd that prints nothing has still given the prompt: it is not
// run again on the host. the script's dir is made by the step user before
// the script is pushed into it.
func TestRunPairStepSandboxEmptyPrompt(t *testing.T) {
	box, _ := withStepBox(t)
	dir := t.TempDir()
	cfg := parseCfg(t, `
sbs: {kind: sandbox, items: {sxx: {image: s}, too: {image: tools}}}
axx: {kind: agent, items: {axx: {harness: opencode-openrouter, model: m, sandbox: sxx}}}
txx: {kind: task, items: {txx: {step-sandbox: too, prompt-cmd: 'echo x >> "$PATS_OUTPUT_DIR/ran"'}}}
sux: {kind: suite, items: {sux: {agents: axx, tasks: txx}}}
`)
	pinAll(t, dir, cfg)
	var out bytes.Buffer
	runDir, _ := Run(cfg, Options{ConfigDir: dir, Now: time.Now(), Out: &out})
	ran, err := os.ReadFile(filepath.Join(runDir, "axx", "txx", "ran"))
	require.NoError(t, err, out.String())
	assert.Equal(t, string(ran), "x\n")
	var mkdir bool
	for _, c := range box.execs {
		if len(c.Argv) > 0 && c.Argv[0] == "mkdir" {
			mkdir = !c.Root
		}
	}
	assert.That(t, mkdir, "the script's dir was not made by the step user")
}
