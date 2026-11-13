package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/lczyk/pats/internal/eval"
	"github.com/lczyk/pats/internal/machine"
)

// HarnessCommand groups the commands about one harness.
type HarnessCommand struct {
	Parse HarnessParseCommand `command:"parse" description:"run a harness's parse script over a stdout log: the summary, the reply, each line it failed on"`
	Pin   HarnessPinCommand   `command:"pin" description:"fetch a cli at a version and print its pin: the hash of its artefact for each arch"`
}

// HarnessPinCommand prints the pin of a cli's version.
type HarnessPinCommand struct {
	Args struct {
		CLI     string `positional-arg-name:"cli"`
		Version string `positional-arg-name:"version"`
	} `positional-args:"yes" required:"yes"`
}

func (c *HarnessPinCommand) Execute(args []string) error {
	tables, err := harnessTables()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return eval.PinCLI(ctx, tables, c.Args.CLI, c.Args.Version, nil, os.Stdout)
}

// HarnessParseCommand tries a parse script against a recorded log.
type HarnessParseCommand struct {
	Args struct {
		Harness string `positional-arg-name:"harness" required:"yes"`
		Log     string `positional-arg-name:"log" description:"a stdout.log (default: stdin)"`
	} `positional-args:"yes"`
}

func (c *HarnessParseCommand) Execute(args []string) error {
	tables, err := harnessTables()
	if err != nil {
		return err
	}
	hd := tables.Harness(c.Args.Harness)
	if hd == nil {
		return fmt.Errorf("unknown harness %q (%s)", c.Args.Harness, strings.Join(tables.Names(), ", "))
	}
	var in io.Reader = os.Stdin
	if c.Args.Log != "" {
		f, err := os.Open(c.Args.Log)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	return eval.ParseLog(hd, in, os.Stdout)
}

// harnessTables is the harnesses of the config in reach, or with no config
// there the machine's.
func harnessTables() (*machine.Tables, error) {
	if _, err := os.Stat(opts.Config); errors.Is(err, fs.ErrNotExist) {
		return machine.Load(opts.MachineFile)
	}
	cfg, err := load(opts.Config)
	if err != nil {
		return nil, err
	}
	return cfg.Harnesses, nil
}
