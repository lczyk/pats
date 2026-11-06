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
	fmt.Fprintln(tw, "  PAIR\tSTATUS\tTIME\tTURNS\tIN\tOUT\tCOST\tLINES\tTOOLS\tNET\tBYTES\tDENIED\tBLOCKED")

	var totTime, totCost float64
	var totTurns, totIn, totOut, totLines, totTools, totNet, totDenied, totBlocked int
	var totBytes int64
	for _, m := range metas {
		var pm pairMeta
		if b, err := os.ReadFile(m); err != nil || json.Unmarshal(b, &pm) != nil {
			continue
		}
		dir := filepath.Dir(m)
		lines, _ := countLines(filepath.Join(dir, "stdout.log"))
		ea := readEgressAudit(filepath.Join(dir, "egress.log"))
		net, nbytes := ea.Requests, ea.In+ea.Out

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
		totBytes += nbytes
		totDenied += len(pm.DeniedEgress)
		totBlocked += len(pm.BlockedEgress)

		fmt.Fprintf(tw, "  %s/%s\t%s\t%.1fs\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%s\t%d\t%d\n",
			pm.Agent, pm.Task, pm.Status, pm.DurationS, turns, in, out, cost, lines, tools, net, humanBytes(nbytes),
			len(pm.DeniedEgress), len(pm.BlockedEgress))
	}
	fmt.Fprintf(tw, "  total\t\t%.1fs\t%d\t%d\t%d\t$%.3f\t%d\t%d\t%d\t%s\t%d\t%d\n",
		totTime, totTurns, totIn, totOut, totCost, totLines, totTools, totNet, humanBytes(totBytes), totDenied, totBlocked)
	tw.Flush()
}

// humanBytes renders a byte count with a binary suffix: 512, 1.5k, 3.2M.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprint(n)
	case n < 1<<20:
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
}
