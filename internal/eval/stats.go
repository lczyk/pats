package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
)

// statsReport prints a per-pair table of run mechanics -- status, wall time,
// turns, tokens, cost, output/tool/net counts, denied egress -- from each
// pair's metadata.json, plus a totals row. pairs without metadata (crashed
// before the write) are skipped; harnesses without a log parser show dashes
// in the summary-derived columns.
func statsReport(w io.Writer, runDir string) {
	metas, _ := filepath.Glob(filepath.Join(runDir, "*", "*", "metadata.json"))
	if len(metas) == 0 {
		return
	}

	fmt.Fprintln(w, "\nrun stats:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  PAIR\tSTATUS\tTIME\tTURNS\tIN\tOUT\tCOST\tLINES\tTOOLS\tNET\tDENIED")

	var totTime, totCost float64
	var totTurns, totIn, totOut, totLines, totTools, totNet, totDenied int
	for _, m := range metas {
		var pm pairMeta
		if b, err := os.ReadFile(m); err != nil || json.Unmarshal(b, &pm) != nil {
			continue
		}
		dir := filepath.Dir(m)
		lines, _ := countLines(filepath.Join(dir, "stdout.log"))
		net, _ := countLines(filepath.Join(dir, "egress.log"))

		turns, in, out, cost, tools := "-", "-", "-", "-", "-"
		if s := pm.Summary; s != nil {
			nTools := 0
			for _, n := range s.Tools {
				nTools += n
			}
			turns, in, out = fmt.Sprint(s.NumTurns), fmt.Sprint(s.InputTokens), fmt.Sprint(s.OutputTokens)
			if s.HasCost {
				cost = fmt.Sprintf("$%.3f", s.CostUSD)
			}
			tools = fmt.Sprint(nTools)
			totTurns += s.NumTurns
			totIn += s.InputTokens
			totOut += s.OutputTokens
			totCost += s.CostUSD
			totTools += nTools
		}
		totTime += pm.DurationS
		totLines += lines
		totNet += net
		totDenied += len(pm.DeniedEgress)

		fmt.Fprintf(tw, "  %s/%s\t%s\t%.1fs\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%d\n",
			pm.Agent, pm.Task, pm.Status, pm.DurationS, turns, in, out, cost, lines, tools, net, len(pm.DeniedEgress))
	}
	fmt.Fprintf(tw, "  total\t\t%.1fs\t%d\t%d\t%d\t$%.3f\t%d\t%d\t%d\t%d\n",
		totTime, totTurns, totIn, totOut, totCost, totLines, totTools, totNet, totDenied)
	tw.Flush()
}
