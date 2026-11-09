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
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/eval"
	"github.com/lczyk/pats/internal/version"
	"github.com/lczyk/pats/src/sandbox"
)

// Options is the global command structure parsed by go-flags. Config is
// global (`pats -c <path> <command>`); go-flags also accepts it after the
// command name.
type Options struct {
	Config  string `long:"config" short:"c" default:"pats.yaml" description:"path to pats.yaml"`
	Verbose []bool `long:"verbose" short:"v" description:"init, run and score: more detail (build steps, stats, each score); twice also streams builds, host scripts, the agent's stderr and scorers"`

	Init   InitCommand   `command:"init" description:"make and pin the images the suites' sandboxes need"`
	Run    RunCommand    `command:"run" description:"run the suites (agents x tasks)"`
	Score  ScoreCommand  `command:"score" description:"score the most recent run (tasks x scorers)"`
	Report ReportCommand `command:"report" description:"reprint the score report of a past run"`
	List   ListCommand   `command:"list" description:"list configured sandboxes, agents, tasks, scorers, or suites"`
	Egress EgressCommand `command:"egress" description:"the egress proxy: check a policy, read an audit, serve it"`
	Clean  CleanCommand  `command:"clean" description:"remove what dead runs left behind: containers, instances, networks, temp dirs"`
}

// CleanCommand sweeps leftovers of crashed runs.
type CleanCommand struct {
	DryRun    bool          `long:"dry-run" short:"n" description:"print what would go, remove nothing"`
	OlderThan time.Duration `long:"older-than" default:"24h" description:"keep temp dirs younger than this (a live run may own them)"`
	All       bool          `long:"all" short:"a" description:"also remove runs this clean cannot judge (another user's or machine's on a shared daemon), lxd's shared uplink, every image pats built or pulled, and temp dirs of any age; never a run whose lease is fresh"`
	Force     bool          `long:"force" short:"f" description:"--all, and also live runs, the containers using a pats image, and every temp dir and lease: everything pats left"`
	Run       string        `long:"run" description:"remove this one run (its token), whatever its lease"`
	JSON      bool          `long:"json" description:"one json object a line for each thing removed, kept or left, in place of the log (with -n: what would go)"`
	LXDRemote []string      `long:"lxd-remote" description:"lxd remote to sweep (repeatable); default: the lxc cli's default remote, when lxc is installed"`
}

func (c *CleanCommand) Execute(args []string) error {
	return eval.Clean(context.Background(), eval.CleanOptions{OlderThan: c.OlderThan, DryRun: c.DryRun, All: c.All, Force: c.Force, Run: c.Run, LXDRemotes: c.LXDRemote, JSON: c.JSON, Out: os.Stdout})
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
		Verbose:   len(opts.Verbose),
	})
}

// RunCommand runs the suites.
type RunCommand struct {
	Jobs    int      `long:"jobs" short:"j" default:"1" description:"max pairs to run in parallel; -1 for auto"`
	Agents  []string `long:"agent" short:"a" description:"only run these agents or agent vectors (repeatable); default: all"`
	Tasks   []string `long:"task" short:"t" description:"only run these tasks or task vectors (repeatable); default: all"`
	Suites  []string `long:"suite" short:"S" description:"only run these suites or suite vectors (repeatable); default: all"`
	Offline bool     `long:"offline" description:"skip checking for newer upstream images and the openrouter model check"`
	DryRun  bool     `long:"dry-run" short:"n" description:"list the pairs with a cost estimate from past runs, and run nothing"`
	JSON    bool     `long:"json" description:"with -n: the list as one json object a line on stdout, the log on stderr"`
	Repeat  int      `long:"repeat" short:"N" default:"1" description:"run the selection this many times, one run after another, each its own run dir"`
	MaxCost float64  `long:"max-cost" value-name:"<usd>" description:"stop starting pairs once the reported spend reaches this many us dollars, across every repeat"`
}

func (r *RunCommand) Execute(args []string) error {
	if r.JSON && !r.DryRun {
		return errors.New("--json goes with -n/--dry-run only")
	}
	if r.Repeat < 1 {
		return errors.New("--repeat must be at least 1")
	}
	if r.MaxCost != 0 || flagSet("run", "max-cost") {
		if err := config.CheckCost(r.MaxCost); err != nil {
			return fmt.Errorf("--max-cost: %w", err)
		}
	}
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	out := io.Writer(os.Stdout)
	if r.JSON {
		out = os.Stderr
	}
	_, err = eval.Run(cfg, eval.Options{
		ConfigDir: filepath.Dir(opts.Config),
		Now:       time.Now(),
		Out:       out,
		DryRun:    r.DryRun,
		Repeat:    r.Repeat,
		MaxCost:   r.MaxCost,
		JSON:      r.JSON,
		JSONOut:   os.Stdout,
		Jobs:      r.Jobs,
		Agents:    r.Agents,
		Tasks:     r.Tasks,
		Suites:    r.Suites,
		Offline:   r.Offline,
		Verbose:   len(opts.Verbose),
	})
	return err
}

// ScoreCommand scores a run (the latest by default).
type ScoreCommand struct {
	Run     string   `long:"run" short:"r" description:"run to score: a dir, a friendly name like fluffy-bunny, a number (1 = run 001; 0 = latest, -1 = second to last; default: latest), or all"`
	Jobs    int      `long:"jobs" short:"j" default:"1" description:"max scores to compute in parallel; -1 for auto"`
	Agents  []string `long:"agent" short:"a" description:"only score these agents' pairs, or agent vectors' (repeatable); default: all"`
	Tasks   []string `long:"task" short:"t" description:"only score these tasks' pairs, or task vectors' (repeatable); default: all"`
	Scorers []string `long:"scorer" short:"s" description:"only run these scorers or scorer vectors (repeatable); default: all"`
	Suites  []string `long:"suite" short:"S" description:"only score these suites or suite vectors (repeatable); default: all"`
	Runs    int      `long:"runs" short:"N" description:"score the latest this many runs, each on its own"`
}

func (s *ScoreCommand) Execute(args []string) error {
	if err := checkRuns("score", s.Runs, s.Run); err != nil {
		return err
	}
	cfg, err := load(opts.Config)
	if err != nil {
		return err
	}
	_, err = eval.Score(cfg, eval.ScoreOptions{
		ConfigDir: filepath.Dir(opts.Config),
		RunDir:    s.Run,
		Jobs:      s.Jobs,
		Suites:    s.Suites,
		Agents:    s.Agents,
		Tasks:     s.Tasks,
		Scorers:   s.Scorers,
		Runs:      s.Runs,
		Out:       os.Stdout,
		Verbose:   len(opts.Verbose),
	})
	return err
}

// ReportCommand reprints a run's report from its scores.json (the latest run
// by default). reads run artifacts only -- no config load, like `list runs`.
type ReportCommand struct {
	Run  string `long:"run" short:"r" description:"run to report: a dir, a friendly name like fluffy-bunny, or a number (1 = run 001; 0 = latest, -1 = second to last; default: latest)"`
	JSON bool   `long:"json" description:"print the run's results as one json document, for scripts, and nothing else"`
	Runs int    `long:"runs" short:"N" description:"report the latest this many runs together: mean, sd and n per pair"`
}

func (c *ReportCommand) Execute(args []string) error {
	if err := checkRuns("report", c.Runs, c.Run); err != nil {
		return err
	}
	return eval.Report(filepath.Dir(opts.Config), c.Run, c.Runs, c.JSON, os.Stdout, os.Stderr)
}

// checkRuns is -N's rules: a whole number of at least one when given, and
// never beside -r.
func checkRuns(command string, n int, run string) error {
	switch {
	case n < 0, n == 0 && flagSet(command, "runs"):
		return errors.New("--runs must be at least 1")
	case n > 0 && run != "":
		return errors.New("--runs and --run do not combine")
	}
	return nil
}

// flagSet says whether command's --long was given, in any spelling the
// parser takes (-N3, -vN 3, --runs=3), so an explicit 0 is told from the
// flag's absence.
func flagSet(command, long string) bool {
	if parser == nil {
		return false
	}
	c := parser.Find(command)
	if c == nil {
		return false
	}
	o := c.FindOptionByLongName(long)
	return o != nil && o.IsSet()
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

// parser is the command line's parser, kept so a command can ask whether a
// flag was given (flagSet).
var parser *flags.Parser

// newParser is pats's command line over opts.
func newParser() *flags.Parser {
	p := flags.NewParser(&opts, flags.Default&^flags.PrintErrors)
	p.Name = "pats"
	p.Usage = "[OPTIONS] COMMAND"
	return p
}

// wantsVersion reports whether args (os.Args[1:]) ask for the version.
// only the first argument counts -- otherwise "pats score -r -v" would print
// the version instead of reporting a bad value for -r. a lone -v is the
// version too; followed by a command it is verbosity.
func wantsVersion(args []string) bool {
	switch {
	case len(args) == 0:
		return false
	case args[0] == "--version" || args[0] == "version":
		return true
	}
	return len(args) == 1 && args[0] == "-v"
}

func main() {
	// handle --version before parsing -- go-flags would otherwise demand a command.
	if wantsVersion(os.Args[1:]) {
		fmt.Println(version.Info)
		os.Exit(0)
	}

	// PrintErrors stripped so we can colourise the help text ourselves.
	parser = newParser()

	_, err := parser.Parse()
	// every command's leases end with it: a temp dir or a container's lease
	// file, an lxd run's marker. run and init end theirs themselves.
	sandbox.Shutdown()
	if err != nil {
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
		if errors.Is(err, eval.ErrInterrupted) {
			os.Exit(130)
		}
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
