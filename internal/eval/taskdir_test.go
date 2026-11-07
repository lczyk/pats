package eval

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// a task from a task.yaml: its scripts run in its own dir with bin/ on PATH,
// its env and PATS_TASK_DIR exported, its inline prompt staged verbatim; a
// scorer of it sees PATS_TASK_DIR too.
func TestRunTaskDir(t *testing.T) {
	var staged string
	fake := &fakeSandbox{onRun: func(spec sandbox.Spec) {
		data, _ := os.ReadFile(filepath.Join(homeMountHost(spec), promptName))
		staged = string(data)
	}}
	old := newSandbox
	newSandbox = func(o sandbox.Options) (sandbox.Sandbox, error) { return fake, nil }
	defer func() { newSandbox = old }()

	root := tree(t, map[string]string{
		"cases/knockout/curl/task.yaml": `prompt: |
  slice ${task} please
env:
  BRANCH: ubuntu-26.04
stage: |
  stage-truth
collect-cmd: ./collect.sh
`,
		"cases/knockout/curl/truth.yaml": "expected\n",
		"bin/stage-truth": "#!/bin/sh\n" +
			"cp truth.yaml \"$PATS_OUTPUT_DIR/expected.yaml\"\n" +
			"printf '%s %s %s\\n' \"$(basename \"$PWD\")\" \"$BRANCH\" \"$PATS_TASK_DIR\" > \"$PATS_OUTPUT_DIR/prepare-env\"\n",
		"cases/knockout/curl/collect.sh": "#!/bin/sh\ntouch \"$PATS_OUTPUT_DIR/collected\"\n",
		"score-dir.sh":                   "#!/bin/sh\n[ \"$PATS_TASK_DIR\" = \"$PWD/cases/knockout/curl\" ] && [ -f \"$PATS_TASK_DIR/truth.yaml\" ] && echo 1.0 || echo 0.0\n",
	})
	for _, f := range []string{"bin/stage-truth", "cases/knockout/curl/collect.sh", "score-dir.sh"} {
		require.NoError(t, os.Chmod(filepath.Join(root, f), 0o755))
	}
	cfg, err := config.ParseIn(root, []byte(`
s: {kind: sandbox, items: {s: {image: img}}}
a: {kind: agent, items: {a: {harness: opencode-openrouter, model: m, sandbox: s}}}
knockout: {kind: task, dir: cases/knockout}
sc: {kind: scorer, items: {dir-seen: {score-cmd: ./score-dir.sh}}}
su: {kind: suite, items: {su: {agents: a, tasks: knockout, scorers: sc}}}
`))
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())

	var out bytes.Buffer
	runDir, err := Run(cfg, Options{ConfigDir: root, Now: time.Now(), Out: &out})
	require.NoError(t, err, out.String())
	outDir := filepath.Join(runDir, "a", "curl")

	assert.Equal(t, staged, "slice ${task} please\n")
	env, err := os.ReadFile(filepath.Join(outDir, "prepare-env"))
	require.NoError(t, err, out.String())
	taskDir := filepath.Join(root, "cases", "knockout", "curl")
	assert.Equal(t, string(env), "curl ubuntu-26.04 "+taskDir+"\n")
	_, err = os.Stat(filepath.Join(outDir, "expected.yaml"))
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(outDir, "collected"))
	assert.NoError(t, err)

	rep, err := Score(cfg, ScoreOptions{ConfigDir: root, RunDir: runDir, Out: &out})
	require.NoError(t, err, out.String())
	assert.Equal(t, rep.PerPair["a/curl"], 1.0)
}

// tree writes files (path -> content) under a fresh temp dir and returns it.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	return root
}
