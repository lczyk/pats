package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/lczyk/pats/internal/config"
)

// list helpers print one line per configured entity, tab-aligned. read-only:
// no lock, no disk writes.

// ListAgents prints id, kind, model, resolved sandbox, and effort per agent.
func ListAgents(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tKIND\tMODEL\tSANDBOX\tEFFORT")
	for _, a := range cfg.Agents {
		sb, err := cfg.ResolveSandbox(a)
		if err != nil {
			sb = "?" // dangling/ambiguous sandbox ref; validate would have caught it
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.ResolvedModel(), sb, a.Effort)
	}
	return w.Flush()
}

// ListTasks prints id and prompt per task.
func ListTasks(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tPROMPT")
	for _, t := range cfg.Tasks {
		fmt.Fprintf(w, "%s\t%s\n", t.ID, t.ResolvedPrompt())
	}
	return w.Flush()
}

// ListSandboxes prints id, kind, resolved driver, image, and egress mode.
func ListSandboxes(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SANDBOX\tKIND\tDRIVER\tIMAGE\tEGRESS")
	for _, s := range cfg.Sandboxes {
		mode := s.Egress.Mode
		if mode == "" {
			mode = "open" // the default: no egress filtering
		}
		image := s.Image
		if image == "" && s.Build != "" {
			image = "build:" + s.Build
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Kind, s.ResolvedDriver(), image, mode)
	}
	return w.Flush()
}

// ListScorers prints id, kind, and the source (exec file or agent id).
func ListScorers(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCORER\tKIND\tSOURCE")
	for _, s := range cfg.Scorers {
		kind, src := s.Kind, s.Score
		switch s.Kind {
		case "":
			kind, src = "exec", s.ExecFile()
		case "agent":
			src = s.AgentID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.ID, kind, src)
	}
	return w.Flush()
}

// ListSuites prints id and axis sizes per suite, plus the pair counts its
// cross-products expand to.
func ListSuites(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SUITE\tAGENTS\tTASKS\tSCORERS\tRUN PAIRS\tSCORE PAIRS")
	for _, s := range cfg.Suites {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\n",
			s.ID, len(s.Agents), len(s.Tasks), len(s.Scorers),
			len(s.Agents)*len(s.Tasks), len(s.Tasks)*len(s.Scorers))
	}
	return w.Flush()
}

// ListRuns prints one line per run dir under .pats/runs (oldest first): the run
// name, how many pairs it has + a status tally, and the overall score if scored.
// it reads run artifacts only -- no pats.yaml needed, so it works on a broken config.
func ListRuns(configDir string, out io.Writer) error {
	base := filepath.Join(configDir, runsSubdir)
	names, err := sortedRunNames(base)
	if err != nil {
		return nil // no runs yet
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RUN\tPAIRS\tSTATUS\tSCORE")
	for _, name := range names {
		runDir := filepath.Join(base, name)
		tally, n := runStatusTally(runDir)
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", name, n, tally, runScore(runDir))
	}
	return w.Flush()
}

// runStatusTally counts pair metadata.json files under a run and tallies their
// status (e.g. "7 ok, 2 error"). returns the tally and the pair count.
func runStatusTally(runDir string) (string, int) {
	metas, _ := filepath.Glob(filepath.Join(runDir, "*", "*", "metadata.json"))
	counts := map[string]int{}
	for _, m := range metas {
		var pm pairMeta
		if b, err := os.ReadFile(m); err == nil && json.Unmarshal(b, &pm) == nil {
			counts[pm.Status]++
		}
	}
	var parts []string
	for _, s := range []string{"ok", "nonzero", "error"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	if len(parts) == 0 {
		return "-", len(metas)
	}
	return strings.Join(parts, ", "), len(metas)
}

// runScore returns the run's overall score (2dp) from scores.json, or "-" if it
// hasn't been scored.
func runScore(runDir string) string {
	b, err := os.ReadFile(filepath.Join(runDir, "scores.json"))
	if err != nil {
		return "-"
	}
	var r ScoreReport
	if json.Unmarshal(b, &r) != nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", r.Overall)
}
