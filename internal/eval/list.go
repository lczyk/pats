package eval

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/lczyk/pats/internal/config"
)

// list helpers print one line per configured entity, tab-aligned. read-only:
// no lock, no disk writes.

// ListAgents prints id, kind, model, resolved sandbox, and effort per agent.
func ListAgents(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, a := range cfg.Agents {
		sb, err := cfg.ResolveSandbox(a)
		if err != nil {
			sb = "?" // dangling/ambiguous sandbox ref; validate would have caught it
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Kind, a.Model, sb, a.Effort)
	}
	return w.Flush()
}

// ListTasks prints id and prompt-file per task.
func ListTasks(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, t := range cfg.Tasks {
		fmt.Fprintf(w, "%s\t%s\n", t.ID, t.PromptFile)
	}
	return w.Flush()
}

// ListSandboxes prints id, kind, resolved driver, image, and egress mode.
func ListSandboxes(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, s := range cfg.Sandboxes {
		mode := s.Egress.Mode
		if mode == "" {
			mode = "off" // default per docs/proposals/network-egress.md
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Kind, s.ResolvedDriver(), s.Image, mode)
	}
	return w.Flush()
}

// ListScorers prints id, kind, and the source (bash file or agent id).
func ListScorers(cfg *config.Config, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, s := range cfg.Scorers {
		src := s.File
		if s.Kind == "agent" {
			src = s.AgentID
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.ID, s.Kind, src)
	}
	return w.Flush()
}
