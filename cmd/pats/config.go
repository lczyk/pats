package main

import (
	"os"

	"github.com/lczyk/pats/internal/machine"
)

// ConfigCommand prints the machine's file: the tables pats runs with here
// (docs/spec/machine-file.md).
type ConfigCommand struct {
	Defaults bool `long:"defaults" description:"print the defaults this pats ships, as they are written, whatever this machine's file does to them"`
}

func (c *ConfigCommand) Execute(args []string) error {
	if c.Defaults {
		_, err := os.Stdout.Write(machine.DefaultsYAML())
		return err
	}
	tables, err := machine.Load(opts.MachineFile)
	if err != nil {
		return err
	}
	return tables.Print(os.Stdout)
}
