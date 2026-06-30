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
	ver "github.com/lczyk/version/go"
)

// Options is the global command structure parsed by go-flags.
type Options struct {
	Run   RunCommand   `command:"run" description:"run the test-matrix (agents x tasks)"`
	Score ScoreCommand `command:"score" description:"score the most recent run (tasks x scorers)"`
}

// RunCommand runs the test-matrix.
type RunCommand struct {
	Config string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
	Jobs   int    `long:"jobs" short:"j" default:"1" description:"max pairs to run in parallel; -1 for auto"`
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
	})
	return err
}

// ScoreCommand scores a run (the latest by default).
type ScoreCommand struct {
	Config  string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
	Run     string `long:"run" short:"r" description:"run dir to score (default: latest under .pats/runs)"`
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
		Agentic:   s.Agentic,
		Out:       os.Stdout,
	})
	return err
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
			fmt.Println(ver.FormatVersion(version.Version, version.CommitSHA, version.BuildDate, version.BuildInfo))
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
