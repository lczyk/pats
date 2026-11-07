// Command pats runs score-based suites of agents over tasks, then scores the
// outputs. see the repo README + pats.example.yaml.
//
// `run` executes every suite's (agent, task) pairs in a sandbox and collects
// outputs into a run dir; `score` runs each suite's tasks x scorers over a run
// and aggregates.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/eval"
	"github.com/lczyk/pats/internal/version"
)

// Options is the global command structure parsed by go-flags. Config is
// global (`pats -c <path> <command>`); go-flags also accepts it after the
// command name.
type Options struct {
	Config string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`

	Init   InitCommand   `command:"init" description:"make and pin the images the suites' sandboxes need"`
	Run    RunCommand    `command:"run" description:"run the suites (agents x tasks)"`
	Score  ScoreCommand  `command:"score" description:"score the most recent run (tasks x scorers)"`
	Report ReportCommand `command:"report" description:"reprint the score report of a past run"`
	List   ListCommand   `command:"list" description:"list configured sandboxes, agents, tasks, scorers, or suites"`
	Egress EgressCommand `command:"egress" description:"the egress proxy: check a policy, read an audit, serve it"`
	Clean  CleanCommand  `command:"clean" description:"remove what crashed runs left behind: sidecars, networks, temp dirs"`
}

// CleanCommand sweeps leftovers of crashed runs.
type CleanCommand struct {
	DryRun    bool          `long:"dry-run" short:"n" description:"print what would go, remove nothing"`
	OlderThan time.Duration `long:"older-than" default:"24h" description:"keep temp dirs younger than this (a live run may own them)"`
	Force     bool          `long:"force" description:"also remove containers and networks another user or host made, and lxd runs whose lease is live"`
	LXDRemote []string      `long:"lxd-remote" description:"lxd remote to sweep (repeatable); default: the lxc cli's default remote, when lxc is installed"`
}

func (c *CleanCommand) Execute(args []string) error {
	return eval.Clean(context.Background(), eval.CleanOptions{OlderThan: c.OlderThan, DryRun: c.DryRun, Force: c.Force, LXDRemotes: c.LXDRemote, Out: os.Stdout})
}

// InitCommand makes and pins the sandboxes' images.
type InitCommand struct {
	Agents    []string `long:"agent" short:"a" description:"only the sandboxes these agents or agent vectors use (repeatable); default: all"`
	Tasks     []string `long:"task" short:"t" description:"only the sandboxes these tasks' pairs use (repeatable); default: all"`
	Suites    []string `long:"suite" short:"S" description:"only the sandboxes these suites or suite vectors use (repeatable); default: all"`
	Sandboxes []string `long:"sandbox" description:"only these sandboxes (repeatable); default: every one the selection uses"`
	Driver    string   `long:"driver" description:"only sandboxes of this driver"`
	Force     bool     `long:"force" description:"rebuild and re-pull what already exists"`
}

func (c *InitCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	return eval.Init(cfg, eval.InitOptions{
		ConfigDir: filepath.Dir(opts.Config),
		Out:       os.Stdout,
		Suites:    c.Suites,
		Agents:    c.Agents,
		Tasks:     c.Tasks,
		Sandboxes: c.Sandboxes,
		Driver:    c.Driver,
		Force:     c.Force,
	})
}

// RunCommand runs the suites.
type RunCommand struct {
	Jobs   int      `long:"jobs" short:"j" default:"1" description:"max pairs to run in parallel; -1 for auto"`
	Agents []string `long:"agent" short:"a" description:"only run these agents or agent vectors (repeatable); default: all"`
	Tasks  []string `long:"task" short:"t" description:"only run these tasks or task vectors (repeatable); default: all"`
	Suites []string `long:"suite" short:"S" description:"only run these suites or suite vectors (repeatable); default: all"`
}

func (r *RunCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	_, err = eval.Run(cfg, eval.Options{
		ConfigDir: filepath.Dir(opts.Config),
		Now:       time.Now(),
		Out:       os.Stdout,
		Jobs:      r.Jobs,
		Agents:    r.Agents,
		Tasks:     r.Tasks,
		Suites:    r.Suites,
	})
	return err
}

// ScoreCommand scores a run (the latest by default).
type ScoreCommand struct {
	Run     string   `long:"run" short:"r" description:"run to score: a dir, a friendly name like fluffy-bunny, a number (1 = run 001; 0 = latest, -1 = second to last; default: latest), or all"`
	Jobs    int      `long:"jobs" short:"j" default:"1" description:"max scorer cells to run in parallel; -1 for auto"`
	Agentic bool     `long:"agentic" description:"also run agent-kind scorers"`
	Agents  []string `long:"agent" short:"a" description:"only score these agents' pairs, or agent vectors' (repeatable); default: all"`
	Tasks   []string `long:"task" short:"t" description:"only score these tasks' pairs, or task vectors' (repeatable); default: all"`
	Scorers []string `long:"scorer" short:"s" description:"only run these scorers or scorer vectors (repeatable); default: all"`
	Suites  []string `long:"suite" short:"S" description:"only score these suites or suite vectors (repeatable); default: all"`
}

func (s *ScoreCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	_, err = eval.Score(cfg, eval.ScoreOptions{
		ConfigDir: filepath.Dir(opts.Config),
		RunDir:    s.Run,
		Jobs:      s.Jobs,
		Agentic:   s.Agentic,
		Suites:    s.Suites,
		Agents:    s.Agents,
		Tasks:     s.Tasks,
		Scorers:   s.Scorers,
		Out:       os.Stdout,
	})
	return err
}

// ReportCommand reprints a run's report from its scores.json (the latest run
// by default). reads run artifacts only -- no config load, like `list runs`.
type ReportCommand struct {
	Run string `long:"run" short:"r" description:"run to report: a dir, a friendly name like fluffy-bunny, or a number (1 = run 001; 0 = latest, -1 = second to last; default: latest)"`
}

func (c *ReportCommand) Execute(args []string) error {
	return eval.Report(filepath.Dir(opts.Config), c.Run, os.Stdout)
}

// ListCommand groups the per-kind list subcommands.
type ListCommand struct {
	Sandboxes ListSandboxesCommand `command:"sandboxes" description:"list configured sandboxes"`
	Agents    ListAgentsCommand    `command:"agents" description:"list configured agents"`
	Tasks     ListTasksCommand     `command:"tasks" description:"list configured tasks"`
	Scorers   ListScorersCommand   `command:"scorers" description:"list configured scorers"`
	Suites    ListSuitesCommand    `command:"suites" description:"list configured suites"`
	Runs      ListRunsCommand      `command:"runs" description:"list past runs under .pats/runs"`
}

type ListSandboxesCommand struct{}
type ListAgentsCommand struct{}
type ListTasksCommand struct{}
type ListScorersCommand struct{}
type ListSuitesCommand struct{}
type ListRunsCommand struct{}

func (c *ListSandboxesCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	return eval.ListSandboxes(cfg, os.Stdout)
}
func (c *ListAgentsCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	return eval.ListAgents(cfg, os.Stdout)
}
func (c *ListTasksCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	return eval.ListTasks(cfg, os.Stdout)
}
func (c *ListScorersCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	return eval.ListScorers(cfg, os.Stdout)
}
func (c *ListSuitesCommand) Execute(args []string) error {
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	return eval.ListSuites(cfg, os.Stdout)
}

// runs reads run artifacts only -- no config load, so it works on a broken pats.yaml.
func (c *ListRunsCommand) Execute(args []string) error {
	return eval.ListRuns(filepath.Dir(opts.Config), os.Stdout)
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

// opts is package-level so command Execute methods can read the global flags.
var opts Options

// wantsVersion reports whether args (os.Args[1:]) ask for the version.
// only the first argument counts -- otherwise "pats score -r -v" would print
// the version instead of reporting a bad value for -r, and any future
// subcommand flag spelled -v would be swallowed too.
func wantsVersion(args []string) bool {
	return len(args) > 0 && (args[0] == "--version" || args[0] == "-v")
}

func main() {
	// handle --version before parsing -- go-flags would otherwise demand a command.
	if wantsVersion(os.Args[1:]) {
		fmt.Println(version.Info)
		os.Exit(0)
	}

	// PrintErrors stripped so we can colourise the help text ourselves.
	parser := flags.NewParser(&opts, flags.Default&^flags.PrintErrors)
	parser.Name = "pats"
	parser.Usage = "[OPTIONS] COMMAND"

	if _, err := parser.Parse(); err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			help := flagsErr.Message
			if useColor(os.Stdout) {
				help = colorize(help)
			}
			fmt.Println(help)
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// useColor reports whether f is a terminal and NO_COLOR is unset (no-color.org).
func useColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var (
	helpHeader = regexp.MustCompile(`(?m)^\S.*:$`)                       // "Usage:", "Available commands:", ...
	helpToken  = regexp.MustCompile(`(?m)^(  +)(-\S+(?: --\S+)?|\w\S*)`) // leading flag or command name
)

// colorize bolds section headers and cyans flag/command names in go-flags help text.
func colorize(help string) string {
	help = helpHeader.ReplaceAllString(help, "\x1b[1m$0\x1b[0m")
	return helpToken.ReplaceAllString(help, "$1\x1b[36m$2\x1b[0m")
}
