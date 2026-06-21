// Command pats runs a score-based test matrix of agents over tasks, then
// scores the outputs. see the repo README + pats.example.yaml.
//
// phase 1: `run` and `score` load + validate the config and print the plan.
// the actual sandbox execution + scoring land in later phases.
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
	})
	return err
}

// ScoreCommand scores the most recent run.
type ScoreCommand struct {
	Config  string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
	Agentic bool   `long:"agentic" description:"also run agent-kind scorers"`
}

func (s *ScoreCommand) Execute(args []string) error {
	cfg, err := load(s.Config)
	if err != nil {
		return err
	}
	pairs, err := cfg.ExpandScorerMatrix()
	if err != nil {
		return fmt.Errorf("expand scorer-matrix:\n%w", err)
	}
	fmt.Printf("scorer-matrix: %d pair(s) (agentic=%v)\n", len(pairs), s.Agentic)
	for _, p := range pairs {
		fmt.Printf("  %-24s x %-24s  w=%.2f\n", p.Task, p.Scorer, p.Weight)
	}
	fmt.Fprintln(os.Stderr, "\npats score: scoring not implemented yet (phase 1 prints the plan)")
	return nil
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
