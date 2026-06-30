package eval

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lczyk/pats/internal/config"
)

// ScoreOptions configures the score phase.
type ScoreOptions struct {
	ConfigDir string // dir holding pats.yaml -- scorer paths resolve against it
	RunDir    string // explicit run dir, or "" for the latest under .pats/runs
	Agentic   bool   // also run agent-kind scorers
	Out       io.Writer
}

// ScoreReport is the aggregated result of scoring a run.
type ScoreReport struct {
	RunDir   string             `json:"run_dir"`
	Cells    []ScoreCell        `json:"cells"`
	PerPair  map[string]float64 `json:"per_pair"` // "agent/task" -> mean over scorers
	PerAgent map[string]float64 `json:"per_agent"`
	Overall  float64            `json:"overall"`
}

// ScoreCell is one (agent, task, scorer) result.
type ScoreCell struct {
	Agent  string  `json:"agent"`
	Task   string  `json:"task"`
	Scorer string  `json:"scorer"`
	Score  float64 `json:"score"`
}

// Score runs the scorer-matrix over a run's collected outputs and aggregates.
func Score(cfg *config.Config, opts ScoreOptions) (*ScoreReport, error) {
	// absolute config dir: scorers run with cwd=ConfigDir; their file path +
	// PATS_OUTPUT_DIR must resolve regardless of that cwd.
	if abs, err := filepath.Abs(opts.ConfigDir); err == nil {
		opts.ConfigDir = abs
	}
	unlock, err := lockConfigDir(opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	runDir := opts.RunDir
	if runDir == "" {
		latest, err := latestRunDir(filepath.Join(opts.ConfigDir, runsSubdir))
		if err != nil {
			return nil, err
		}
		runDir = latest
	}
	fmt.Fprintf(opts.Out, "scoring: %s\n", relToCwd(runDir))

	testPairs, err := cfg.ExpandTestMatrix()
	if err != nil {
		return nil, err
	}
	scorePairs, err := cfg.ExpandScorerMatrix()
	if err != nil {
		return nil, err
	}
	scorers := index(cfg.Scorers, func(s config.Scorer) string { return s.ID })
	agentModel := map[string]string{}
	for _, a := range cfg.Agents {
		agentModel[a.ID] = a.Model
	}
	// task -> scorers to run on it.
	byTask := map[string][]config.ScorePair{}
	for _, sp := range scorePairs {
		byTask[sp.Task] = append(byTask[sp.Task], sp)
	}

	var cells []ScoreCell
	for _, tp := range testPairs {
		outDir := filepath.Join(runDir, tp.Agent, tp.Task)
		if _, err := os.Stat(outDir); err != nil {
			continue // pair didn't run / no output
		}
		for _, sp := range byTask[tp.Task] {
			sc := scorers[sp.Scorer]
			if sc.Kind == "agent" && !opts.Agentic {
				continue // agentic scorers gated behind --agentic
			}
			score, serr := runScorer(opts, sc, outDir, tp.Agent, tp.Task, agentModel[tp.Agent])
			switch {
			case errors.Is(serr, errScorerNA):
				fmt.Fprintf(opts.Out, "  [%s x %s] %s = n/a\n", tp.Agent, tp.Task, sc.ID)
				continue // not applicable -- dropped from aggregation
			case serr != nil:
				fmt.Fprintf(opts.Out, "  [%s x %s] %s: %v\n", tp.Agent, tp.Task, sc.ID, serr)
				continue
			}
			fmt.Fprintf(opts.Out, "  [%s x %s] %s = %.4f\n", tp.Agent, tp.Task, sc.ID, score)
			cells = append(cells, ScoreCell{tp.Agent, tp.Task, sc.ID, score})
		}
	}

	rep := aggregate(runDir, cells, testPairs)
	report(opts.Out, rep)
	if err := writeJSON(filepath.Join(runDir, "scores.json"), rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// errScorerNA is the silent-skip signal: a scorer printed "na", meaning it
// doesn't apply to this cell. dropped from aggregation, not logged as an error.
var errScorerNA = errors.New("scorer reported n/a")

// runScorer runs one scorer over an output dir and returns its [0,1] score.
// file scorers exec directly on the host (trusted user scripts); the shebang
// picks the interpreter. agent scorers pending.
func runScorer(opts ScoreOptions, sc config.Scorer, outDir, agent, task, model string) (float64, error) {
	switch sc.Kind {
	case "":
		cmd, err := commandFor(opts.ConfigDir, sc.ExecFile(), map[string]string{
			"PATS_OUTPUT_DIR": outDir,
			"PATS_AGENT_ID":   agent,
			"PATS_TASK_ID":    task,
			"PATS_SCORER_ID":  sc.ID,
			"PATS_MODEL":      model,
		})
		if err != nil {
			return 0, err
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr // NOTE: captured but unused for now; TODO(lczyk): persist scorer logs
		if err := cmd.Run(); err != nil {
			return 0, fmt.Errorf("run: %w", err) // non-zero exit = failure
		}
		return parseScore(stdout.String())
	case "agent":
		return 0, fmt.Errorf("agent scorer not implemented yet")
	default:
		return 0, fmt.Errorf("unknown scorer kind %q", sc.Kind)
	}
}

// parseScore reads a scorer's verdict from the first non-empty stdout line: a
// [0,1] float, or "na" -> errScorerNA (a silent skip).
func parseScore(s string) (float64, error) {
	first := ""
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			first = t
			break
		}
	}
	if first == "" {
		return 0, fmt.Errorf("no output")
	}
	if strings.EqualFold(first, "na") {
		return 0, errScorerNA
	}
	f, err := strconv.ParseFloat(first, 64)
	if err != nil {
		return 0, fmt.Errorf("output %q is not a float or na", first)
	}
	if math.IsNaN(f) || f < 0 || f > 1 {
		return 0, fmt.Errorf("score %v out of [0,1]", f)
	}
	return f, nil
}

func aggregate(runDir string, cells []ScoreCell, testPairs []config.TestPair) *ScoreReport {
	// per (agent,task): mean over scorers.
	type acc struct {
		n   int
		sum float64
	}
	pair := map[string]*acc{}
	for _, c := range cells {
		k := c.Agent + "/" + c.Task
		a := pair[k]
		if a == nil {
			a = &acc{}
			pair[k] = a
		}
		a.n++
		a.sum += c.Score
	}
	perPair := map[string]float64{}
	for k, a := range pair {
		if a.n > 0 {
			perPair[k] = a.sum / float64(a.n)
		}
	}

	// per agent: mean over its tasks.
	agent := map[string]*acc{}
	for _, tp := range testPairs {
		k := tp.Agent + "/" + tp.Task
		ts, ok := perPair[k]
		if !ok {
			continue
		}
		a := agent[tp.Agent]
		if a == nil {
			a = &acc{}
			agent[tp.Agent] = a
		}
		a.n++
		a.sum += ts
	}
	perAgent := map[string]float64{}
	var osum float64
	for id, a := range agent {
		if a.n > 0 {
			perAgent[id] = a.sum / float64(a.n)
			osum += perAgent[id]
		}
	}
	overall := 0.0
	if len(perAgent) > 0 {
		overall = osum / float64(len(perAgent))
	}
	return &ScoreReport{
		RunDir: runDir, Cells: cells,
		PerPair: perPair, PerAgent: perAgent, Overall: overall,
	}
}

func report(w io.Writer, r *ScoreReport) {
	fmt.Fprintln(w, "---")
	for _, agent := range sortedKeys(r.PerAgent) {
		fmt.Fprintf(w, "%s  %.2f\n", agent, r.PerAgent[agent])
		// tasks for this agent
		for _, k := range sortedKeys(r.PerPair) {
			a, task, _ := strings.Cut(k, "/")
			if a != agent {
				continue
			}
			fmt.Fprintf(w, "  %-24s %.2f", task, r.PerPair[k])
			var parts []string
			for _, c := range r.Cells {
				if c.Agent == agent && c.Task == task {
					parts = append(parts, fmt.Sprintf("%s=%.2f", c.Scorer, c.Score))
				}
			}
			fmt.Fprintf(w, "  (%s)\n", strings.Join(parts, ", "))
		}
	}
	fmt.Fprintf(w, "overall  %.2f\n", r.Overall)
}

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// latestRunDir returns the highest-sorted run dir under base.
func latestRunDir(base string) (string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("no runs found under %s (run `pats run` first): %w", base, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no runs found under %s", base)
	}
	// sort by (date, numeric suffix) so 20260621-10 beats 20260621-2.
	sort.Slice(names, func(i, j int) bool {
		di, ni := splitRunName(names[i])
		dj, nj := splitRunName(names[j])
		if di != dj {
			return di < dj
		}
		return ni < nj
	})
	return filepath.Join(base, names[len(names)-1]), nil
}

func splitRunName(name string) (date string, n int) {
	date, num, _ := strings.Cut(name, "-")
	n, _ = strconv.Atoi(num)
	return date, n
}
