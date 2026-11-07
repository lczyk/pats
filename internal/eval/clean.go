package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lczyk/pats/src/sandbox"
)

// CleanOptions configures Clean.
type CleanOptions struct {
	Drivers   []string      // container clis to sweep; nil: docker and podman, whichever are installed
	OlderThan time.Duration // leftover temp dirs younger than this are kept
	DryRun    bool
	// Force also removes containers and networks this user and host do not
	// own: another user's on a shared daemon, or ones made before pats
	// labelled its own.
	Force bool
	// LXDRemotes are the lxd servers to sweep; nil: the lxc cli's default
	// remote, when lxc is installed and Drivers is nil.
	LXDRemotes []string
	Out        io.Writer
}

// Clean removes what crashed runs left behind. every agent, sidecar and
// network pats makes is named after its pair's workdir (sbx-agent-<dir>,
// sbx-proxy-<dir>, sbx-egr-<dir>) and labelled with its owner (host and uid).
// one of this owner's is a leftover once the pats process named in its
// workdir's owner file is gone; another owner's cannot be judged from here
// (its workdir is in someone else's cache, or on another machine) and is left
// unless Force. a temp dir is kept while its owner runs, and otherwise goes
// once older than OlderThan.
func Clean(ctx context.Context, o CleanOptions) error {
	tmp, err := sandbox.TempRoot()
	if err != nil {
		return err
	}
	drivers := o.Drivers
	if drivers == nil {
		for _, d := range sandbox.Engines() {
			if _, err := exec.LookPath(d); err == nil {
				drivers = append(drivers, d)
			}
		}
	}
	var errs []error
	act := "removed"
	if o.DryRun {
		act = "would remove"
	}
	owner := "label=" + sandbox.OwnerLabel()
	for _, d := range drivers {
		// list returns every name with the prefix and the set this owner made.
		list := func(kind, prefix string) (all []string, mine map[string]bool, err error) {
			args := []string{"ps", "-a", "--filter", "name=^" + prefix, "--format", "{{.Names}}"}
			if kind == "network" {
				args = []string{"network", "ls", "--filter", "name=^" + prefix, "--format", "{{.Name}}"}
			}
			out, err := exec.CommandContext(ctx, d, args...).Output()
			if err != nil {
				return nil, nil, err
			}
			own, err := exec.CommandContext(ctx, d, append(args[:len(args)-2:len(args)-2], "--filter", owner, "--format", args[len(args)-1])...).Output()
			if err != nil {
				return nil, nil, err
			}
			mine = map[string]bool{}
			for _, n := range strings.Fields(string(own)) {
				mine[n] = true
			}
			return strings.Fields(string(out)), mine, nil
		}
		foreign := 0
		sweep := func(kind, prefix string, rm ...string) error {
			all, mine, err := list(kind, prefix)
			if err != nil {
				return err
			}
			for _, name := range all {
				if !mine[name] && !o.Force {
					foreign++
					continue
				}
				if mine[name] && sandbox.TempOwnerAlive(filepath.Join(tmp, strings.TrimPrefix(name, prefix))) {
					continue
				}
				if !o.DryRun {
					if out, err := exec.CommandContext(ctx, d, append(rm, name)...).CombinedOutput(); err != nil {
						errs = append(errs, fmt.Errorf("%s %s %s: %w (%s)", d, strings.Join(rm, " "), name, err, strings.TrimSpace(string(out))))
						continue
					}
				}
				fmt.Fprintf(o.Out, "%s %s %s %s\n", act, d, kind, name)
			}
			return nil
		}
		// agents first: one still attached keeps its network from going.
		if err := sweep("container", "sbx-agent-", "rm", "-f"); err != nil {
			fmt.Fprintf(o.Out, "%s: skipped (%v)\n", d, err)
			continue
		}
		if err := sweep("container", "sbx-proxy-", "rm", "-f"); err != nil {
			errs = append(errs, fmt.Errorf("%s ps: %w", d, err))
			continue
		}
		if err := sweep("network", "sbx-egr-", "network", "rm"); err != nil {
			errs = append(errs, fmt.Errorf("%s network ls: %w", d, err))
			continue
		}
		if foreign > 0 {
			fmt.Fprintf(o.Out, "%s: left %d not made by %s (another user, another machine, or an older pats); --force removes them\n", d, foreign, sandbox.OwnerLabel())
		}
	}

	remotes := o.LXDRemotes
	if remotes == nil && o.Drivers == nil {
		if _, err := exec.LookPath("lxc"); err == nil {
			remotes = []string{""}
		}
	}
	for _, r := range remotes {
		if err := sandbox.CleanLXD(ctx, r, o.Force, o.DryRun, o.Out); err != nil {
			errs = append(errs, err)
		}
	}

	ents, err := os.ReadDir(tmp)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.Join(errs...)
		}
		return errors.Join(append(errs, err)...)
	}
	for _, e := range ents {
		p := filepath.Join(tmp, e.Name())
		if owner, ok := strings.CutSuffix(p, ".owner"); ok {
			// an owner file outlives its dir when the caller removed the dir.
			if _, err := os.Stat(owner); os.IsNotExist(err) && !o.DryRun {
				_ = os.Remove(p)
			}
			continue
		}
		info, err := e.Info()
		if err != nil || !e.IsDir() || time.Since(info.ModTime()) < o.OlderThan || sandbox.TempOwnerAlive(p) {
			continue
		}
		if !o.DryRun {
			if err := os.RemoveAll(p); err != nil {
				errs = append(errs, err)
				continue
			}
			_ = os.Remove(p + ".owner")
		}
		fmt.Fprintf(o.Out, "%s %s\n", act, p)
	}
	return errors.Join(errs...)
}
