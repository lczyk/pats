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
	Out       io.Writer
}

// Clean removes what crashed runs left behind. every sidecar and network pats
// makes is named after its pair's workdir (sbx-proxy-<dir>, sbx-egr-<dir>),
// and that workdir's owner file names the pats process running the pair, so
// one whose process is gone (or whose workdir is) is a leftover. a temp dir
// is kept while its owner runs, and otherwise goes once older than OlderThan.
func Clean(ctx context.Context, o CleanOptions) error {
	tmp, err := sandbox.TempRoot()
	if err != nil {
		return err
	}
	live := func(name, prefix string) bool {
		return sandbox.TempOwnerAlive(filepath.Join(tmp, strings.TrimPrefix(name, prefix)))
	}
	drivers := o.Drivers
	if drivers == nil {
		for _, d := range []string{"docker", "podman"} {
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
	for _, d := range drivers {
		out, err := exec.CommandContext(ctx, d, "ps", "-a", "--filter", "name=^sbx-proxy-", "--format", "{{.Names}}").Output()
		if err != nil {
			fmt.Fprintf(o.Out, "%s: skipped (%v)\n", d, err)
			continue
		}
		for _, name := range strings.Fields(string(out)) {
			if live(name, "sbx-proxy-") {
				continue
			}
			if !o.DryRun {
				if out, err := exec.CommandContext(ctx, d, "rm", "-f", name).CombinedOutput(); err != nil {
					errs = append(errs, fmt.Errorf("%s rm %s: %w (%s)", d, name, err, strings.TrimSpace(string(out))))
					continue
				}
			}
			fmt.Fprintf(o.Out, "%s %s container %s\n", act, d, name)
		}
		out, err = exec.CommandContext(ctx, d, "network", "ls", "--filter", "name=^sbx-egr-", "--format", "{{.Name}}").Output()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s network ls: %w", d, err))
			continue
		}
		for _, name := range strings.Fields(string(out)) {
			if live(name, "sbx-egr-") {
				continue
			}
			if !o.DryRun {
				if out, err := exec.CommandContext(ctx, d, "network", "rm", name).CombinedOutput(); err != nil {
					errs = append(errs, fmt.Errorf("%s network rm %s: %w (%s)", d, name, err, strings.TrimSpace(string(out))))
					continue
				}
			}
			fmt.Fprintf(o.Out, "%s %s network %s\n", act, d, name)
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
