package eval

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
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
	PerPair  map[string]float64 `json:"per_pair"` // "agent/task" -> weighted mean over scorers
	PerAgent map[string]float64 `json:"per_agent"`
	Overall  float64            `json:"overall"`
}

// ScoreCell is one (agent, task, scorer) result.
type ScoreCell struct {
	Agent  string  `json:"agent"`
	Task   string  `json:"task"`
	Scorer string  `json:"scorer"`
	Score  float64 `json:"score"`
	Weight float64 `json:"weight"`
}

// Score runs the scorer-matrix over a run's collected outputs and aggregates.
func Score(cfg *config.Config, opts ScoreOptions) (*ScoreReport, error) {
	runDir := opts.RunDir
	if runDir == "" {
		latest, err := latestRunDir(filepath.Join(opts.ConfigDir, runsSubdir))
		if err != nil {
			return nil, err
		}
		runDir = latest
	}
	fmt.Fprintf(opts.Out, "scoring: %s\n", runDir)

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
	// task -> scorers (with weight) to run on it.
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
			if serr != nil {
				fmt.Fprintf(opts.Out, "  [%s x %s] scorer %s: %v\n", tp.Agent, tp.Task, sc.ID, serr)
				continue
			}
			cells = append(cells, ScoreCell{tp.Agent, tp.Task, sc.ID, score, sp.Weight})
		}
	}

	rep := aggregate(runDir, cells, testPairs)
	report(opts.Out, rep)
	if err := writeJSON(filepath.Join(runDir, "scores.json"), rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// runScorer runs one scorer over an output dir and returns its [0,1] score.
// bash scorers run on the host (trusted user scripts); agent scorers pending.
func runScorer(opts ScoreOptions, sc config.Scorer, outDir, agent, task, model string) (float64, error) {
	switch sc.Kind {
	case "bash":
		cmd := exec.Command("sh", filepath.Join(opts.ConfigDir, sc.File))
		cmd.Dir = opts.ConfigDir
		cmd.Env = append(os.Environ(),
			"PATS_OUTPUT_DIR="+outDir,
			"PATS_AGENT="+agent,
			"PATS_TASK="+task,
			"PATS_SCORER="+sc.ID,
			"PATS_MODEL="+model,
		)
		out, err := cmd.Output()
		if err != nil {
			return 0, fmt.Errorf("run: %w", err)
		}
		return parseScore(string(out))
	case "agent":
		return 0, fmt.Errorf("agent scorer not implemented yet")
	default:
		return 0, fmt.Errorf("unknown scorer kind %q", sc.Kind)
	}
}

// parseScore reads a [0,1] float from the scorer's last non-empty output line.
func parseScore(s string) (float64, error) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			last = t
			break
		}
	}
	if last == "" {
		return 0, fmt.Errorf("no output")
	}
	f, err := strconv.ParseFloat(last, 64)
	if err != nil {
		return 0, fmt.Errorf("output %q is not a float", last)
	}
	if math.IsNaN(f) || f < 0 || f > 1 {
		return 0, fmt.Errorf("score %v out of [0,1]", f)
	}
	return f, nil
}

func aggregate(runDir string, cells []ScoreCell, testPairs []config.TestPair) *ScoreReport {
	// per (agent,task): weighted mean over scorers.
	type acc struct{ wsum, sum float64 }
	pair := map[string]*acc{}
	for _, c := range cells {
		k := c.Agent + "/" + c.Task
		a := pair[k]
		if a == nil {
			a = &acc{}
			pair[k] = a
		}
		a.wsum += c.Weight
		a.sum += c.Weight * c.Score
	}
	perPair := map[string]float64{}
	for k, a := range pair {
		if a.wsum > 0 {
			perPair[k] = a.sum / a.wsum
		}
	}

	// per agent: weighted mean over tasks, using test-matrix weight.
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
		a.wsum += tp.Weight
		a.sum += tp.Weight * ts
	}
	perAgent := map[string]float64{}
	var osum float64
	for id, a := range agent {
		if a.wsum > 0 {
			perAgent[id] = a.sum / a.wsum
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
