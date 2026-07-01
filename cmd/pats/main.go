// Command pats runs a score-based test matrix of agents over tasks, then
// scores the outputs. see the repo README + pats.example.yaml.
//
// `run` executes every test-matrix pair in a sandbox and collects outputs into
// a run dir; `score` runs the scorer-matrix over a run and aggregates.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/eval"
	"github.com/lczyk/pats/internal/version"
)

// Options is the global command structure parsed by go-flags.
type Options struct {
	Run   RunCommand   `command:"run" description:"run the test-matrix (agents x tasks)"`
	Score ScoreCommand `command:"score" description:"score the most recent run (tasks x scorers)"`
	List  ListCommand  `command:"list" description:"list configured sandboxes, agents, tasks, or scorers"`
}

// RunCommand runs the test-matrix.
type RunCommand struct {
	Config string   `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
	Jobs   int      `long:"jobs" short:"j" default:"1" description:"max pairs to run in parallel; -1 for auto"`
	Agents []string `long:"agent" short:"a" description:"only run these agents (repeatable); default: all"`
	Tasks  []string `long:"task" short:"t" description:"only run these tasks (repeatable); default: all"`
}

func (r *RunCommand) Execute(args []string) error {
	cfg, err := load(r.Config)
	if err != nil {
		return err
	}
	_, err = eval.Run(cfg, eval.Options{
		ConfigDir: filepath.Dir(r.Config),
		Now:       time.Now(),
		Out:       os.Stdout,
		Jobs:      r.Jobs,
		Agents:    r.Agents,
		Tasks:     r.Tasks,
	})
	return err
}

// ScoreCommand scores a run (the latest by default).
type ScoreCommand struct {
	Config  string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
	Run     string `long:"run" short:"r" description:"run dir to score (default: latest under .pats/runs)"`
	Jobs    int    `long:"jobs" short:"j" default:"1" description:"max scorer cells to run in parallel; -1 for auto"`
	Agentic bool   `long:"agentic" description:"also run agent-kind scorers"`
}

func (s *ScoreCommand) Execute(args []string) error {
	cfg, err := load(s.Config)
	if err != nil {
		return err
	}
	_, err = eval.Score(cfg, eval.ScoreOptions{
		ConfigDir: filepath.Dir(s.Config),
		RunDir:    s.Run,
		Jobs:      s.Jobs,
		Agentic:   s.Agentic,
		Out:       os.Stdout,
	})
	return err
}

// ListCommand groups the per-vector list subcommands.
type ListCommand struct {
	Sandboxes ListSandboxesCommand `command:"sandboxes" description:"list configured sandboxes"`
	Agents    ListAgentsCommand    `command:"agents" description:"list configured agents"`
	Tasks     ListTasksCommand     `command:"tasks" description:"list configured tasks"`
	Scorers   ListScorersCommand   `command:"scorers" description:"list configured scorers"`
	Runs      ListRunsCommand      `command:"runs" description:"list past runs under .pats/runs"`
}

type ListSandboxesCommand struct {
	Config string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
}
type ListAgentsCommand struct {
	Config string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
}
type ListTasksCommand struct {
	Config string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
}
type ListScorersCommand struct {
	Config string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
}
type ListRunsCommand struct {
	Config string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
}

func (c *ListSandboxesCommand) Execute(args []string) error {
	cfg, err := load(c.Config)
	if err != nil {
		return err
	}
	return eval.ListSandboxes(cfg, os.Stdout)
}
func (c *ListAgentsCommand) Execute(args []string) error {
	cfg, err := load(c.Config)
	if err != nil {
		return err
	}
	return eval.ListAgents(cfg, os.Stdout)
}
func (c *ListTasksCommand) Execute(args []string) error {
	cfg, err := load(c.Config)
	if err != nil {
		return err
	}
	return eval.ListTasks(cfg, os.Stdout)
}
func (c *ListScorersCommand) Execute(args []string) error {
	cfg, err := load(c.Config)
	if err != nil {
		return err
	}
	return eval.ListScorers(cfg, os.Stdout)
}

// runs reads run artifacts only -- no config load, so it works on a broken pats.yaml.
func (c *ListRunsCommand) Execute(args []string) error {
	return eval.ListRuns(filepath.Dir(c.Config), os.Stdout)
}

func load(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s:\n%w", path, err)
	}
	return cfg, nil
}

func main() {
	// handle --version before parsing -- go-flags would otherwise demand a command.
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-v" {
			fmt.Println(version.Info)
			os.Exit(0)
		}
	}

	var opts Options
	parser := flags.NewParser(&opts, flags.Default)
	parser.Name = "pats"
	parser.Usage = "[OPTIONS] COMMAND"

	if _, err := parser.Parse(); err != nil {
		// go-flags already prints the error.
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		os.Exit(1)
	}
}
