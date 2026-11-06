package eval

import (
	"context"
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
// and a live pair's workdir exists, so one whose workdir is gone is a
// leftover. temp dirs under the cache have no such owner, so they go by age.
func Clean(ctx context.Context, o CleanOptions) error {
	tmp, err := sandbox.TempRoot()
	if err != nil {
		return err
	}
	live := func(name, prefix string) bool {
		_, err := os.Stat(filepath.Join(tmp, strings.TrimPrefix(name, prefix)))
		return err == nil
	}
	drivers := o.Drivers
	if drivers == nil {
		for _, d := range []string{"docker", "podman"} {
			if _, err := exec.LookPath(d); err == nil {
				drivers = append(drivers, d)
			}
		}
	}
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
				_ = exec.CommandContext(ctx, d, "rm", "-f", name).Run()
			}
			fmt.Fprintf(o.Out, "%s %s container %s\n", act, d, name)
		}
		out, err = exec.CommandContext(ctx, d, "network", "ls", "--filter", "name=^sbx-egr-", "--format", "{{.Name}}").Output()
		if err != nil {
			continue
		}
		for _, name := range strings.Fields(string(out)) {
			if live(name, "sbx-egr-") {
				continue
			}
			if !o.DryRun {
				_ = exec.CommandContext(ctx, d, "network", "rm", name).Run()
			}
			fmt.Fprintf(o.Out, "%s %s network %s\n", act, d, name)
		}
	}

	ents, err := os.ReadDir(tmp)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < o.OlderThan {
			continue
		}
		p := filepath.Join(tmp, e.Name())
		if !o.DryRun {
			if err := os.RemoveAll(p); err != nil {
				fmt.Fprintf(o.Out, "could not remove %s: %v\n", p, err)
				continue
			}
		}
		fmt.Fprintf(o.Out, "%s %s\n", act, p)
	}
	return nil
}
