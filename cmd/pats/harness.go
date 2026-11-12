package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/lczyk/pats/internal/eval"
	"github.com/lczyk/pats/internal/machine"
)

// HarnessCommand groups the commands about one harness.
type HarnessCommand struct {
	Parse HarnessParseCommand `command:"parse" description:"run a harness's parse script over a stdout log: the summary, the reply, each line it failed on"`
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
		return machine.Load()
	}
	cfg, err := load(opts.Config)
	if err != nil {
		return nil, err
	}
	return cfg.Harnesses, nil
}
