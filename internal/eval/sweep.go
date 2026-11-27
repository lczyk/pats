package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// sweepBound caps the sweep at a run's start: every list, removal and the
// lock wait.
var sweepBound = 60 * time.Second

// NoSweepEnv set to 1 skips the sweep at a run's start.
const NoSweepEnv = "PATS_NO_SWEEP"

// sweepDead stops what certainly dead runs of this machine left on the
// places the selection uses (docs/spec/run-sweep.md). it never fails the
// run: a place it cannot reach, a removal that fails or the bound reached
// is a warning.
func sweepDead(ctx context.Context, cfg *config.Config, names []string, dryRun bool, lg logw) {
	if os.Getenv(NoSweepEnv) == "1" {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, sweepBound)
	defer cancel()
	unlock, err := machineLock(sctx)
	if err != nil {
		lg.warn("sweep: the machine lock: %v; pats clean removes what dead runs left", err)
		return
	}
	defer unlock()
	sandboxes := index(cfg.Sandboxes, func(s config.Sandbox) string { return s.Name })
	// one line a run, whatever places it was on: said at the end, cut short
	// or not.
	swept := map[string]map[string]int{} // run: kind: count
	defer func() {
		for _, run := range sweptRuns(swept) {
			what := sweptText(swept[run])
			if dryRun {
				lg.detail("sweep: would stop what run %s left (its process is gone): %s", run, what)
			} else {
				lg.info("stopped what run %s left (its process is gone): %s; pats clean removes the rest", run, what)
			}
		}
	}()
	report := func(e sandbox.CleanEvent) {
		if swept[e.Run] == nil {
			swept[e.Run] = map[string]int{}
		}
		swept[e.Run][e.Kind]++
	}
	seen := map[string]bool{}
	for _, n := range names {
		sb, ok := sandboxes[n]
		if !ok {
			continue
		}
		o := placeOptions(sb)
		key := o.Driver + " " + o.Remote
		if seen[key] {
			continue
		}
		seen[key] = true
		place := o.Driver
		if o.Remote != "" {
			place += " " + o.Remote
		}
		d, err := openDriver(o)
		if err != nil {
			lg.warn("sweep: %s: %v", place, err)
			continue
		}
		err = sandbox.Sweep(sctx, d, sandbox.SweepOptions{Place: place, DryRun: dryRun, Report: report})
		if errors.Is(sctx.Err(), context.DeadlineExceeded) {
			lg.warn("sweep: cut short after %s at %s; pats clean removes what is left", sweepBound, place)
			return
		}
		if err != nil {
			lg.warn("sweep: %s: %v; pats clean removes what is left", place, err)
		}
	}
}

// sweptText is a run's swept resources, kind by kind.
func sweptText(kinds map[string]int) string {
	var parts []string
	for _, k := range []string{"sandbox", "network", "acl"} {
		if n := kinds[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	return strings.Join(parts, ", ")
}

func sweptRuns(m map[string]map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sweepNames are the sandboxes the selection starts: its agents', and its
// tasks' and scorers' step sandboxes.
func sweepNames(cfg *config.Config, pairs []config.TestPair, judged ...config.Scorer) []string {
	agents := index(cfg.Agents, func(a config.Agent) string { return a.Name })
	tasks := index(cfg.Tasks, func(t config.Task) string { return t.Name })
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		if one, err := cfg.One("sandbox", name); err == nil {
			out = append(out, one)
		}
	}
	for _, p := range pairs {
		if id, err := cfg.ResolveSandbox(agents[p.Agent]); err == nil {
			out = append(out, id)
		}
		add(tasks[p.Task].StepSandbox)
	}
	for _, sc := range judged {
		add(sc.Task().StepSandbox)
	}
	return out
}
