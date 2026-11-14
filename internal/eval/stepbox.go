package eval

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// a step box is a step sandbox started for one phase of a pair, or for a
// run's scoring: the steps run in it at their host paths, with the tree the
// pats root sits in read-only and only what the phase may write writable.
// see docs/spec/step-sandbox.md.

// stepHome is the step user's HOME in a box: made at start, not a bind, so
// nothing syncs back from it.
const stepHome = "/tmp/pats-step-home"

// stepBox is one started step sandbox.
type stepBox struct {
	sess    sandbox.Session
	driver  string
	staging string // a pats temp dir for what pats writes while the box holds the binds
	work    string // the box's unused workdir
	sb      config.Sandbox
	// kill: end the step user's leftovers after each step from inside the
	// step's own exec (docker, podman, lxd, as a non-root user).
	kill bool
	// broken: an exec failed to run, so the box is not lent again.
	broken bool
}

// stepPlace is a resolved step sandbox: its config, and the item's
// step-env.
type stepPlace struct {
	sb  config.Sandbox
	env []string
}

// stepPlaceOf is where an item's host steps run: nil on the host.
func stepPlaceOf(cfg *config.Config, sandboxes map[string]config.Sandbox, stepSandbox string, stepEnv []string) (*stepPlace, error) {
	if stepSandbox == "" {
		return nil, nil
	}
	name, err := cfg.One("sandbox", stepSandbox)
	if err != nil {
		return nil, fmt.Errorf("step-sandbox: %w", err)
	}
	return &stepPlace{sb: sandboxes[name], env: stepEnv}, nil
}

// realHome is the user's own home, which a strict snap moves $HOME away
// from.
func realHome() string {
	if h := os.Getenv("SNAP_REAL_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// resolved is p with links resolved as far as it exists.
func resolved(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir = filepath.Clean(dir); dir == p {
		return p
	}
	return filepath.Join(resolved(dir), base)
}

// within says whether p is dir or under it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// stepTree is the tree bound read-only into a step box: the work tree that
// holds root, found by its first .git (a dir, or a submodule's or linked
// worktree's file) below the user's home, else root alone. a root that is
// the home or above it cannot use a step sandbox at all.
func stepTree(root string) (string, error) {
	home := realHome()
	rootR, homeR := resolved(root), resolved(home)
	if home != "" && within(rootR, homeR) {
		return "", fmt.Errorf("the pats root %s is the home or above it: a step sandbox would see the whole home", root)
	}
	for d := root; ; {
		if home != "" && within(resolved(d), homeR) {
			return root, nil
		}
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		up := filepath.Dir(d)
		if up == d {
			return root, nil
		}
		d = up
	}
}

// stepMasks are the paths under tree a step box sees empty: the runs dir and
// the cache dirs, at their paths and at where they resolve, both as box
// paths under tree.
func stepMasks(tree, root string) []string {
	cands := []string{filepath.Join(root, ".pats")}
	if c, err := sandbox.CacheDir(); err == nil {
		cands = append(cands, c)
	}
	if l := sandbox.LegacyCacheDir(); l != "" {
		cands = append(cands, l)
	}
	treeR := resolved(tree)
	var out []string
	add := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, c := range cands {
		if within(tree, c) && c != tree {
			add(c)
		}
		if r := resolved(c); within(treeR, r) && r != treeR {
			rel, _ := filepath.Rel(treeR, r)
			add(filepath.Join(tree, rel))
		}
	}
	return out
}

// stepBindsFor are a box's mounts at their host paths: the tree read-only
// with its masks, then the phase's writable dirs and read-only extras, each
// after any it lies under.
func stepBindsFor(root string, rw, ro []string) ([]sandbox.Mount, error) {
	tree, err := stepTree(root)
	if err != nil {
		return nil, err
	}
	ms := []sandbox.Mount{{Host: tree, Container: tree, ReadOnly: true, Masks: stepMasks(tree, root)}}
	for _, p := range ro {
		ms = append(ms, sandbox.Mount{Host: p, Container: p, ReadOnly: true})
	}
	for _, p := range rw {
		ms = append(ms, sandbox.Mount{Host: p, Container: p})
	}
	slices.SortStableFunc(ms, func(a, b sandbox.Mount) int {
		return strings.Count(filepath.Clean(a.Container), "/") - strings.Count(filepath.Clean(b.Container), "/")
	})
	for _, m := range ms {
		for _, r := range sandbox.ReservedPaths() {
			if within(r, m.Container) {
				return nil, fmt.Errorf("%s would bind at or under %s, which pats keeps for itself in a sandbox", m.Container, r)
			}
		}
	}
	return ms, nil
}

// killsLeftovers says whether a step's own exec ends the step user's other
// processes as it returns: under docker and podman unless pats runs as root
// (the step user is then root, and the box's keepalive too), always under
// lxd, whose agent user is never root, never under bwrap, whose pid namespace
// dies with each exec. a var: a test on the host must never send it.
var killsLeftovers = func(driver string) bool {
	switch {
	case driver == "bwrap":
		return false
	case driver == sandbox.LXDDriver:
		return true
	}
	return os.Getuid() != 0
}

// remapCheck says whether a step sandbox's engine maps uids elsewhere, so it
// cannot be one; swappable in tests, which have no engine.
var remapCheck = func(ctx context.Context, sb config.Sandbox) (bool, error) {
	d, err := sandbox.Open(placeOptions(sb))
	if err != nil {
		return false, err
	}
	r, ok := d.(interface {
		RemapsUIDs(context.Context) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return r.RemapsUIDs(ctx)
}

// openStepBox starts p's sandbox with these mounts and checks, as the step
// user, that it sees and reaches every one before any step runs. a start is
// tried up to three times when its push met a file that shrank, each try on
// one setupTimeout that covers the engine check, the start and the checks.
// audit names the box's egress audit, under its staging dir; probe is a file
// the tree must show (the loaded config).
func openStepBox(ctx context.Context, p *stepPlace, mounts []sandbox.Mount, audit, probe string) (*stepBox, error) {
	var err error
	for try := 1; try <= 3; try++ {
		var b *stepBox
		if b, err = openStepBoxOnce(ctx, p, mounts, audit, probe); err == nil {
			return b, nil
		}
		if !errors.Is(err, sandbox.ErrChanged) || ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("step sandbox %q: %w", p.sb.Name, err)
}

func openStepBoxOnce(ctx context.Context, p *stepPlace, mounts []sandbox.Mount, audit, probe string) (_ *stepBox, err error) {
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	if remaps, err := remapCheck(ctx, p.sb); err != nil {
		return nil, err
	} else if remaps {
		return nil, errors.New("its engine maps the host uid to another (rootless, or userns-remap), so a step could not write through its binds as the user")
	}
	staging, err := sandbox.MkTempFor(p.sb.Driver, "pats-steps-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			sandbox.RemoveTemp(staging)
		}
	}()
	// the box's workdir lives as long as the box: bwrap binds it afresh on
	// every exec.
	work, err := sandbox.MkTemp("pats-stepwork-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			sandbox.RemoveTemp(work)
		}
	}()
	box, err := newSandbox(placeOptions(p.sb), sandbox.SandboxOptions{
		Image: p.sb.Image, Trees: p.sb.Trees, Profiles: p.sb.Profiles, Config: p.sb.Config, ProxyImage: p.sb.ProxyImage,
		Limits: sandbox.Limits{CPU: p.sb.Limits.CPU, Memory: p.sb.Limits.Memory, Disk: p.sb.Limits.Disk},
	})
	if err != nil {
		return nil, err
	}
	spec := sandbox.Spec{
		Workdir: work, Mounts: mounts, Sudo: p.sb.Sudo,
		Egress: egressFor(p.sb, config.TaskEgress{}, nil, filepath.Join(staging, audit), false),
	}
	bindMounts := p.sb.Driver != sandbox.LXDDriver
	var sentinels []string
	if bindMounts {
		for _, m := range mounts {
			if m.ReadOnly {
				continue
			}
			s := filepath.Join(m.Host, ".pats-visible-"+randHex())
			if err := os.WriteFile(s, nil, 0o600); err != nil {
				return nil, err
			}
			sentinels = append(sentinels, s)
		}
	}
	defer func() {
		for _, s := range sentinels {
			_ = os.Remove(s)
		}
	}()
	sess, err := box.Start(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	b := &stepBox{sess: sess, driver: p.sb.Driver, staging: staging, work: work, sb: p.sb, kill: killsLeftovers(p.sb.Driver)}
	if !bindMounts {
		if u, ok := sess.(interface{ AgentUID() string }); ok && u.AgentUID() == "0" {
			sess.Close()
			return nil, errors.New("its image's pats user is uid 0: a step's cleanup would reach root")
		}
	}
	// the step sandbox's sudo: as for an agent, set once root is done.
	if err := sess.HandOver(ctx); err != nil {
		sess.Close()
		return nil, err
	}
	if err := b.prepare(ctx, mounts, sentinels, probe, 3); err != nil {
		sess.Close()
		return nil, err
	}
	return b, nil
}

// prepare makes the step user's HOME and checks every mount from inside:
// the tree by probe, another read-only mount by an entry listed on the host
// just now, a writable one by its sentinel, each reachable. an entry a host
// step removed meanwhile is listed again, up to tries times.
func (b *stepBox) prepare(ctx context.Context, mounts []sandbox.Mount, sentinels []string, probe string, tries int) error {
	// the tree under lxd is root's copy: git refuses a repo it does not
	// own, a submodule's included, unless told the box trusts them all.
	var checks []string
	for _, m := range mounts {
		checks = append(checks, "x", m.Container)
		// the tree is the one mount with masks (stepBindsFor).
		tree := len(m.Masks) > 0
		switch {
		case tree && probe != "":
			checks = append(checks, "e", probe)
		case tree:
			// a masked entry would show even in an empty tree: its mask
			// makes it.
			if e := firstEntry(m.Host, m.Masks...); e != "" {
				checks = append(checks, "e", filepath.Join(m.Container, e))
			}
		case m.ReadOnly:
			if e := firstEntry(m.Host); e != "" {
				checks = append(checks, "e", filepath.Join(m.Container, e))
			}
		}
	}
	for _, s := range sentinels {
		checks = append(checks, "e", s)
	}
	script := `home=$1; shift
mkdir -p "$home" && printf '[safe]\n\tdirectory = *\n' > "$home/.gitconfig" || exit 1
while [ $# -gt 1 ]; do
	case $1 in
	x) [ -d "$2" ] && [ -x "$2" ] || { echo "cannot reach $2"; exit 3; };;
	e) [ -e "$2" ] || [ -L "$2" ] || { echo "cannot see $2"; exit 3; };;
	esac
	shift 2
done`
	var out strings.Builder
	code, err := b.sess.Exec(ctx, sandbox.Cmd{Argv: append([]string{"/bin/sh", "-c", script, "check", stepHome}, checks...), Dir: "/"}, &out, &out)
	if err != nil {
		return err
	}
	if code != 0 {
		// an entry a host step removed meanwhile is gone from both sides.
		if strings.HasPrefix(out.String(), "cannot see ") {
			missing := strings.TrimSpace(strings.TrimPrefix(out.String(), "cannot see "))
			for _, m := range mounts {
				if within(m.Container, missing) {
					if _, err := os.Lstat(filepath.Join(m.Host, strings.TrimPrefix(missing, m.Container))); errors.Is(err, os.ErrNotExist) && tries > 1 {
						return b.prepare(ctx, mounts, sentinels, probe, tries-1)
					}
				}
			}
		}
		return fmt.Errorf("%s (a daemon that cannot reach a host path mounts it empty: under the docker snap, keep projects under the home)", strings.TrimSpace(out.String()))
	}
	return nil
}

// firstEntry is a name in dir whose path is none of skip, "" when there is
// none or dir is unreadable.
func firstEntry(dir string, skip ...string) string {
	es, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range es {
		if !slices.Contains(skip, filepath.Join(dir, e.Name())) {
			return e.Name()
		}
	}
	return ""
}

// configProbe is the file a step box's tree must show: the pats.yaml in
// the config dir, "" when there is none (a config named otherwise falls
// back to an entry of the tree).
func configProbe(configDir string) string {
	p := filepath.Join(configDir, "pats.yaml")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// stepWrapper runs a step's script as on the host, then ends what it left
// running, from inside the same exec, before the exec returns: no tty in a
// box makes a process group, so the step user's processes are the step's.
// args: the script's dir, the pats root, the dir to run in, how to run it
// (x: exec it, sh: /bin/sh -e), whether to kill.
const stepWrapper = `sd=$1 root=$2 dir=$3 how=$4 kill=$5
PATH="$root/bin:$PATH"; export PATH
cd "$dir" || exit 1
if [ "$how" = x ]; then "$sd/script"; else /bin/sh -e "$sd/script"; fi
rc=$?
[ "$kill" = 1 ] && kill -KILL -1 2>/dev/null
rm -rf "$sd"
exit $rc`

// run runs one step in the box, as the step user, with env on top of the
// step sandbox's own env, its step-env and the box's HOME. its output goes
// to stdout and stderr; a non-zero exit is an error, as on the host.
func (b *stepBox) run(ctx context.Context, p *stepPlace, s step, root string, env map[string]string, stdout, stderr io.Writer) error {
	f, err := os.CreateTemp(b.staging, "script-")
	if err != nil {
		return err
	}
	_, werr := f.WriteString(s.script)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	defer os.Remove(f.Name())
	if werr != nil {
		return werr
	}
	how := "sh"
	if strings.HasPrefix(s.script, "#!") {
		how = "x"
		if err := os.Chmod(f.Name(), 0o755); err != nil {
			return err
		}
	} else if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	// the step user makes the script's dir, so a push that needs it there
	// (docker cp) finds it, and the wrapper may remove it.
	sd := "/tmp/pats-step-" + randHex()
	if code, err := b.sess.Exec(ctx, sandbox.Cmd{Argv: []string{"mkdir", "-p", sd}, Dir: "/"}, io.Discard, io.Discard); err != nil || code != 0 {
		b.broken = err != nil
		return fmt.Errorf("make the script's dir: exit %d: %v", code, err)
	}
	if err := b.sess.Push(ctx, f.Name(), sd+"/script"); err != nil {
		return fmt.Errorf("push the script: %w", err)
	}
	e := maps.Clone(b.sb.Env)
	if e == nil {
		e = map[string]string{}
	}
	for _, k := range p.env {
		if v, ok := os.LookupEnv(k); ok {
			e[k] = v
		}
	}
	maps.Copy(e, env)
	e["PATS_ROOT"], e["HOME"] = root, stepHome
	kill := "0"
	if b.kill {
		kill = "1"
	}
	code, err := b.sess.Exec(ctx, sandbox.Cmd{Argv: []string{"/bin/sh", "-c", stepWrapper, "step", sd, root, s.dir, how, kill}, Env: e, Dir: s.dir}, stdout, stderr)
	if err != nil {
		b.broken = true
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if code != 0 {
		return fmt.Errorf("exit status %d", code)
	}
	return nil
}

// output runs a step and returns its stdout, its stderr's tail in the error
// when it fails, as step.output does on the host.
func (b *stepBox) output(ctx context.Context, p *stepPlace, s step, root string, env map[string]string, tee io.Writer) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	var so, se io.Writer = &stdout, &stderr
	if tee != nil {
		so, se = io.MultiWriter(&stdout, tee), io.MultiWriter(&stderr, tee)
	}
	if err := b.run(ctx, p, s, root, env, so, se); err != nil {
		return nil, fmt.Errorf("%w%s", err, stderrTail(&stderr))
	}
	return stdout.Bytes(), nil
}

// sync brings the writable binds home, on a budget of its own.
func (b *stepBox) sync(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	return b.sess.Sync(sctx)
}

// close ends the box and moves what was staged into dest ("" drops it).
func (b *stepBox) close(dest string) error {
	_ = b.sess.Close()
	defer sandbox.RemoveTemp(b.staging)
	defer sandbox.RemoveTemp(b.work)
	if dest == "" {
		return nil
	}
	es, err := os.ReadDir(b.staging)
	if err != nil {
		return err
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "script-") {
			continue
		}
		if err := moveFile(filepath.Join(b.staging, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// moveFile renames src to dst, copying where they are on different
// filesystems (the cache and a project often are).
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}
