package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/lczyk/pats/internal/agent"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/machine"
	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// harness is one agent invocation wired into a sandbox: the box, the spec
// (argv, env, mounts, egress) and the writable HOME its creds were copied
// into. the HOME lives until cleanup, so a caller can seed it before run and
// read it back after.
type harness struct {
	box     sandbox.Sandbox
	spec    sandbox.Spec
	homeDir string        // host side of paths.Home
	paths   sandbox.Paths // what the agent is told: the in-sandbox paths, or under host the dirs themselves
	cleanup func()
}

// promptName is the prompt file's name inside the harness HOME. it lives there,
// not in the workdir, so the agent's cwd holds only what stage put in it: a
// stray prompt.txt next to the fixture gets noticed and commented on.
const promptName = "prompt.txt"

// newHarness resolves agent a, on harness entry hd, into sandbox sb, with te
// (the task's egress additions; zero for preflight) folded into the policy. the caller writes the prompt to
// promptPath() before run; runID "" (preflight) leaves PATS_RUN_NAME unset and
// turns on the egress self-test; auditPath "" turns the egress audit log off;
// label prefixes the no-creds warning.
func newHarness(opts Options, a config.Agent, hd *machine.Harness, sb config.Sandbox, te config.TaskEgress, workDir, runID, auditPath, label string) (*harness, error) {
	box, err := newSandbox(placeOptions(sb), sandbox.SandboxOptions{
		Image: sb.Image, Trees: sb.Trees, Profiles: sb.Profiles, Config: sb.Config, ProxyImage: sb.ProxyImage,
		Limits: sandbox.Limits{CPU: sb.Limits.CPU, Memory: sb.Limits.Memory, Disk: sb.Limits.Disk},
	})
	if err != nil {
		return nil, err
	}
	env := maps.Clone(sb.Env)
	if env == nil {
		env = map[string]string{}
	}
	cenv, hasToken := agent.CredEnv(hd)
	maps.Copy(env, cenv) // forward host creds to any task-running agent
	// under a key proxy the agent gets a placeholder for each key, and the
	// pair's proxy the real values.
	var held []proxy.KeyBinding
	var real map[string]string
	if sb.Egress.KeyProxy {
		if held, real, err = holdKeys(hd, env); err != nil {
			return nil, err
		}
	}

	var paths sandbox.Paths
	hs, err := harnessHome(opts, label, hd, env, hasToken, func(dir string) string {
		paths = sandbox.PathsOf(box, workDir, dir)
		return paths.Home
	})
	if err != nil {
		return nil, err
	}
	maps.Copy(env, agent.Env(a, paths.Home+"/"+promptName, paths.Work, runID))
	spec, err := agent.Spec(hd, a, workDir, env)
	if err != nil {
		hs.cleanup()
		return nil, err
	}
	spec.Mounts = hs.mounts
	spec.Egress = egressFor(sb, te, hd, auditPath, runID == "")
	spec.Egress.Policy.Keys, spec.Egress.Keys = held, real
	spec.Sudo = sb.Sudo
	return &harness{box: box, spec: spec, homeDir: hs.dir, paths: paths, cleanup: hs.cleanup}, nil
}

// promptPath is the host path the prompt must be written to before run.
func (h *harness) promptPath() string { return filepath.Join(h.homeDir, promptName) }

// harnessRun is the outcome of one harness.run.
type harnessRun struct {
	code     int
	err      error // launch / driver failure; nil on a plain non-zero exit
	timedOut bool
	dur      float64 // seconds
}

// exec runs the agent in an already started session, bounded by timeout (0 =
// none) so a stuck agent is killed rather than hanging the caller.
func (h *harness) exec(ctx context.Context, sess sandbox.Session, stdout, stderr io.Writer, timeout time.Duration) harnessRun {
	return bounded(ctx, timeout, func(rctx context.Context) (int, error) {
		return sess.Exec(rctx, sandbox.Cmd{Argv: h.spec.Argv, Env: h.spec.Env}, stdout, stderr)
	})
}

func bounded(ctx context.Context, timeout time.Duration, f func(context.Context) (int, error)) harnessRun {
	rctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	t0 := time.Now()
	code, err := f(rctx)
	dur := time.Since(t0).Seconds()

	// decide timeout from rctx.Err(), not err: on deadline the sandbox kills
	// the container, which exits non-zero (137) -- the driver maps any
	// *exec.ExitError to (code, nil), so err is nil on a timed-out run too.
	// ctrl+C (context.Canceled on the parent ctx) is not a timeout. the one
	// exception is a launch failure: sandbox setup (egress in particular) can
	// run under the same deadline, so a slow setup can fail *and* leave rctx
	// expired -- that's an error with nothing to collect, not a timed-out
	// agent, and the real cause belongs in the metadata.
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded) && !errors.Is(err, sandbox.ErrLaunch)
	return harnessRun{code: code, err: err, timedOut: timedOut, dur: dur}
}

// homeMount is the in-sandbox HOME for a harness -- a writable dir the agent
// (running as an arbitrary uid) owns, where its creds + cache live. under
// the host driver the agent is told the dir itself (sandbox.PathsOf).
const homeMount = sandbox.HomeMount

type harnessSetup struct {
	dir     string // host side of homeMount
	mounts  []sandbox.Mount
	cleanup func()
}

// harnessHome gives a harness a writable HOME (the --user uid owns nothing),
// copies its file-based credentials when present, and warns when no supported
// credential source is available. copies are writable for token refreshes but
// discarded after the invocation; pats never modifies the host credential file.
// label ("agent/task") prefixes the warning.
//
// inBox names the home as the agent is told it, given its host dir; nil is
// homeMount.
func harnessHome(opts Options, label string, hd *machine.Harness, env map[string]string, hasToken bool, inBox func(string) string) (harnessSetup, error) {
	homeDir, err := sandbox.MkTemp("pats-home-")
	if err != nil {
		return harnessSetup{}, err
	}
	home := homeMount
	if inBox != nil {
		home = inBox(homeDir)
	}
	env["HOME"] = home
	maps.Copy(env, agent.HomeEnv(hd, home))

	hasCreds := hasToken
	// a token in the env is a way in of its own: a default login file this
	// process may not read (a strict snap) is then no error.
	cf, rel, err := agent.HostCredsFile(hd)
	if err != nil && !(hasToken && os.Getenv(agent.AuthPathVar(hd)) == "") {
		sandbox.RemoveTemp(homeDir)
		return harnessSetup{}, err
	}
	if cf != "" {
		dst := filepath.Join(homeDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			sandbox.RemoveTemp(homeDir)
			return harnessSetup{}, err
		}
		if err := copyFile(cf, dst); err != nil {
			sandbox.RemoveTemp(homeDir)
			return harnessSetup{}, err
		}
		hasCreds = true
	}
	// a harness that names no way of logging in needs none.
	if !hasCreds && hd != nil && (len(hd.AuthEnv) > 0 || hd.LoginFile() != "") {
		logw{opts.Out, opts.Color, opts.Verbose}.warn("%s no creds forwarded (expected %s); the harness may fail on auth", label, agent.CredentialHint(hd))
	}
	return harnessSetup{
		dir:     homeDir,
		mounts:  []sandbox.Mount{{Host: homeDir, Container: home}},
		cleanup: func() { sandbox.RemoveTemp(homeDir) },
	}, nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

// egressFor renders the egress policy one agent run gets: the sandbox's, with
// the task's lists unioned in (the sandbox keeps every scalar), and under an
// allowlisting proxy the harness's own hosts (inference, token refresh), so
// pats.yaml only lists what the task needs. hd is nil for a sandbox no agent
// runs in. selfTest asks the driver to prove the sandbox has no route out
// first (preflight does).
func egressFor(sb config.Sandbox, te config.TaskEgress, hd *machine.Harness, auditPath string, selfTest bool) sandbox.Egress {
	eg := sandbox.Egress{
		None:      sb.Egress.None,
		Set:       sb.Egress.Set,
		Policy:    sb.Egress.Policy,
		Image:     sb.ProxyImage,
		AuditPath: auditPath,
		SelfTest:  selfTest,
	}
	if !eg.Set {
		return eg
	}
	eg.Policy = eg.Policy.Resolved()
	eg.Policy.Allow = mergeEntries(eg.Policy.Allow, te.Allow)
	eg.Policy.Deny = mergeEntries(eg.Policy.Deny, te.Deny)
	if hd == nil {
		return eg
	}
	eg.Policy.Deny = mergeEntries(eg.Policy.Deny, proxy.Lines(hd.Egress.Deny...))
	if eg.Policy.Default != "allow" {
		eg.Policy.Allow = mergeEntries(eg.Policy.Allow, proxy.Lines(hd.Egress.Allow...))
	}
	// the harness's hosts are exempt from the methods posture either way: its
	// inference traffic is not the task's to inspect.
	eg.Policy.Harness = mergeHosts(nil, hd.Egress.Allow)
	// a key proxy's bindings are allow entries of their own: they terminate
	// the bound host, and their method passes a methods posture.
	if sb.Egress.KeyProxy {
		for _, k := range slices.Sorted(maps.Keys(hd.AuthBind)) {
			eg.Policy.Allow = mergeEntries(eg.Policy.Allow, proxy.Lines(hd.AuthBind[k]...))
		}
	}
	return eg
}

// holdKeys swaps each key in env that hd binds for a placeholder of its
// shape, and returns the bindings for the pair's policy and the real values,
// by placeholder, for its proxy. an unset key gets none.
func holdKeys(hd *machine.Harness, env map[string]string) ([]proxy.KeyBinding, map[string]string, error) {
	if hd == nil {
		return nil, nil, nil
	}
	var held []proxy.KeyBinding
	real := map[string]string{}
	for _, k := range hd.AuthEnv {
		v, ok := env[k]
		if !ok || v == "" {
			continue
		}
		bind := hd.AuthBind[k]
		if len(bind) == 0 {
			return nil, nil, fmt.Errorf("key proxy: harness %s binds no endpoint for %s", hd.Name, k)
		}
		ph, err := proxy.Placeholder(v, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("key proxy: %s: %w", k, err)
		}
		env[k], real[ph] = ph, v
		held = append(held, proxy.KeyBinding{Name: k, Placeholder: ph, Bind: bind})
	}
	return held, real, nil
}

// keyNames are the names any harness lists in auth-env: what a host step
// beside a key proxy does not inherit.
func keyNames(cfg *config.Config) []string {
	var out []string
	for _, h := range cfg.Tables().Harnesses {
		for _, k := range h.AuthEnv {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

// keyDrop is keyNames when agent a runs in a sandbox with a key proxy, else
// nothing.
func keyDrop(cfg *config.Config, a config.Agent) []string {
	id, err := cfg.ResolveSandbox(a)
	if err != nil {
		return nil
	}
	for _, s := range cfg.Sandboxes {
		if s.Name == id && s.Egress.KeyProxy {
			return keyNames(cfg)
		}
	}
	return nil
}

// mergeHosts appends extras not already in hosts (exact string match; overlap
// via wildcards is harmless -- the proxy just sees a redundant entry).
//
// hosts is copied before appending: the caller passes cfg.Sandboxes[i].Egress.Allow,
// shared by every concurrent pair. today an in-place append is safe because
// yaml.v3 decodes sequences with cap == len (verified), so the append below
// always reallocates -- but nothing enforces that invariant, and a future
// caller (or yaml.v3 change) could silently start corrupting a sibling pair's
// egress policy.
func mergeHosts(hosts, extras []string) []string {
	seen := map[string]bool{}
	for _, h := range hosts {
		seen[h] = true
	}
	out := append([]string(nil), hosts...)
	for _, e := range extras {
		if !seen[e] {
			out = append(out, e)
		}
	}
	return out
}

// mergeEntries is mergeHosts for a policy's entries: an extra is dropped
// when one written the same way, exceptions and all, is already there.
// entries is copied before appending, for mergeHosts' reason.
func mergeEntries(entries, extras []proxy.RawEntry) []proxy.RawEntry {
	out := append([]proxy.RawEntry(nil), entries...)
	for _, e := range extras {
		if !slices.ContainsFunc(out, e.Same) {
			out = append(out, e)
		}
	}
	return out
}
