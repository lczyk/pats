package eval

import (
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
)

// statsReport prints a per-pair table of run mechanics -- status, wall time,
// turns, tokens, cost, tool / net counts, denied egress -- from each pair's
// metadata.json, plus a totals row. an unknown value is "-" and left out of
// its total; a scored pair whose metadata.json is missing or cannot be read
// shows as status "?", the unreadable ones also warned about on warn.
func statsReport(w io.Writer, runDir string, rep *ScoreReport, warn io.Writer) {
	pairs := readPairs(runDir, rep, warn)
	if len(pairs) == 0 {
		return
	}

	fmt.Fprintln(w, "\nrun stats:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  PAIR\tSTATUS\tTIME\tTURNS\tIN\tOUT\tCOST\tTOOLS\tNET\tDENIED")

	var totTime, totCost float64
	var totTurns, totIn, totOut, totTools, totNet, totDenied int
	num := func(n *int, tot *int) string {
		if n == nil {
			return "-"
		}
		*tot += *n
		return fmt.Sprint(*n)
	}
	for _, r := range pairs {
		name := r.agent + "/" + r.task
		if r.meta == nil {
			fmt.Fprintf(tw, "  %s\t?\t-\t-\t-\t-\t-\t-\t-\t-\n", name)
			continue
		}
		var p reportPair
		p.summaryFields(r.meta)
		ea := readEgressAudit(filepath.Join(runDir, r.agent, r.task, "egress.log"))

		cost, tools := "-", "-"
		if p.CostUSD != nil {
			cost = fmt.Sprintf("$%.3f", *p.CostUSD)
			totCost += *p.CostUSD
		}
		if s := r.meta.Summary; s != nil {
			n := 0
			for _, c := range s.Tools {
				n += c
			}
			tools = fmt.Sprint(n)
			totTools += n
		}
		totTime += r.meta.DurationS
		totNet += ea.Requests
		totDenied += len(r.meta.DeniedEgress)

		fmt.Fprintf(tw, "  %s\t%s\t%.1fs\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
			name, r.meta.Status, r.meta.DurationS, num(p.Turns, &totTurns), num(p.InputTokens, &totIn),
			num(p.OutputTokens, &totOut), cost, tools, ea.Requests, len(r.meta.DeniedEgress))
	}
	fmt.Fprintf(tw, "  total\t\t%.1fs\t%d\t%d\t%d\t$%.3f\t%d\t%d\t%d\n",
		totTime, totTurns, totIn, totOut, totCost, totTools, totNet, totDenied)
	tw.Flush()
}
