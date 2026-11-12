package eval

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox"
)

// list helpers print one line per configured item, tab-aligned, with the
// vector it came from. read-only: no lock, no disk writes.

// ListAgents prints vector, name, harness, model, resolved sandbox, and effort per agent.
func ListAgents(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VECTOR\tAGENT\tHARNESS\tMODEL\tSANDBOX\tEFFORT")
	for _, a := range cfg.Agents {
		sb, err := cfg.ResolveSandbox(a)
		if err != nil {
			sb = "?" // dangling/ambiguous sandbox ref; validate would have caught it
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Vector, a.Name, a.Harness, dash(a.Model), sb, a.Effort)
	}
	return w.Flush()
}

// ListHarnesses prints each harness an agent may name, with its cli, where
// its definition is from, and what it has of a parse script and a model
// catalogue.
func ListHarnesses(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HARNESS\tCLI\tFROM\tPARSE\tMODELS")
	for _, name := range cfg.Harnesses.Names() {
		h := cfg.Harnesses.Harness(name)
		parse := "-"
		if h.Parser() != nil {
			parse = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, dash(h.CLI), h.From, parse, dash(h.Models))
	}
	return w.Flush()
}

// dash is s, or a dash for none.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ListTasks prints vector, name, the directory it was defined in, and where its
// prompt comes from, per task.
func ListTasks(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VECTOR\tTASK\tDIR\tPROMPT")
	for _, t := range cfg.Tasks {
		dir := t.Dir
		if dir == "" {
			dir = "."
		}
		prompt := "inline"
		switch {
		case t.PromptFile != "":
			prompt = "file: " + t.PromptFile
		case t.PromptCmd != "":
			prompt = "cmd: " + t.PromptCmd
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Vector, t.Name, dir, prompt)
	}
	return w.Flush()
}

// ListSandboxes prints vector, name, driver, image, and egress mode per sandbox.
func ListSandboxes(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VECTOR\tSANDBOX\tDRIVER\tIMAGE\tEGRESS")
	for _, s := range cfg.Sandboxes {
		mode := s.Egress.Mode()
		image := s.Image
		switch {
		case image == "" && s.Build != "":
			image = "build:" + s.Build
		case image == "":
			image, _ = sandbox.SandboxImage("") // the published default; a dev build's warning belongs to run, not list
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Vector, s.Name, s.Driver, image, mode)
	}
	return w.Flush()
}

// ListScorers prints vector, name, and the source: inline, the command, or the
// judging agent.
func ListScorers(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VECTOR\tSCORER\tSOURCE")
	for _, s := range cfg.Scorers {
		src := "inline"
		switch {
		case s.IsAgent():
			src = "agent: " + s.Agent
		case !strings.Contains(strings.TrimSpace(s.Score), "\n"):
			src = strings.TrimSpace(s.Score)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Vector, s.Name, src)
	}
	return w.Flush()
}

// ListSuites prints vector, name and axis sizes per suite after expansion, plus
// the pair counts its cross-products expand to.
func ListSuites(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VECTOR\tSUITE\tAGENTS\tTASKS\tSCORERS\tRUN PAIRS\tSCORE PAIRS\tMANUAL")
	for _, s := range cfg.Suites {
		test, _ := cfg.ExpandTestPairs(s.Name)
		score, _ := cfg.ExpandScorePairs(s.Name)
		agents, tasks, scorers := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, p := range test {
			agents[p.Agent], tasks[p.Task] = true, true
		}
		for _, p := range score {
			scorers[p.Scorer] = true
		}
		manual := ""
		if s.Manual {
			manual = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n",
			s.Vector, s.Name, len(agents), len(tasks), len(scorers), len(test), len(score), manual)
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
	pairs := readPairs(runDir, nil, nil)
	counts := map[string]int{}
	for _, p := range pairs {
		if p.meta != nil {
			counts[p.meta.Status]++
		}
	}
	var parts []string
	for _, s := range []string{"ok", "nonzero", "timeout", "budget", "interrupted", "error"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	if len(parts) == 0 {
		return "-", len(pairs)
	}
	return strings.Join(parts, ", "), len(pairs)
}

// runScore returns the run's overall score (2dp) from scores.json, or "-" if it
// hasn't been scored by this pats.
func runScore(runDir string) string {
	r, err := readScores(runDir)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", r.Overall)
}
