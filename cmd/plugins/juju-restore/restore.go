// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/juju/cmd"
	"github.com/juju/loggo"
	"github.com/juju/version"
	"launchpad.net/gnuflag"

	"github.com/juju/juju/cmd/envcmd"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/juju"
)

func main() {
	Main(os.Args)
}

func Main(args []string) {
	ctx, err := cmd.DefaultContext()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if err := juju.InitJujuHome(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(2)
	}
	os.Exit(cmd.Main(newRestoreCommand(), ctx, args[1:]))
}

var logger = loggo.GetLogger("juju.plugins.restore")

func newRestoreCommand() cmd.Command {
	return envcmd.Wrap(&restoreCommand{})
}

const restoreDoc = `
Restore restores a backup created with juju backup
by creating a new juju bootstrap instance and arranging
it so that the existing instances in the environment
talk to it.

It verifies that the existing bootstrap instance is
not running. The given constraints will be used
to choose the new instance.
`

type restoreCommand struct {
	envcmd.EnvCommandBase
	Log             cmd.Log
	Constraints     constraints.Value
	backupFile      string
	showDescription bool
}

func (c *restoreCommand) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "juju-restore",
		Purpose: "Restore a backup made with juju backup",
		Args:    "<backupfile.tar.gz>",
		Doc:     restoreDoc,
	}
}

func (c *restoreCommand) SetFlags(f *gnuflag.FlagSet) {
	f.Var(constraints.ConstraintsValue{Target: &c.Constraints}, "constraints", "set environment constraints")
	f.BoolVar(&c.showDescription, "description", false, "show the purpose of this plugin")
	c.Log.AddFlags(f)
}

func (c *restoreCommand) Init(args []string) error {
	if c.showDescription {
		return cmd.CheckEmpty(args)
	}
	if len(args) == 0 {
		return fmt.Errorf("no backup file specified")
	}
	c.backupFile = args[0]
	return cmd.CheckEmpty(args[1:])
}

func (c *restoreCommand) Run(ctx *cmd.Context) error {
	if c.showDescription {
		fmt.Fprintf(ctx.Stdout, "%s\n", c.Info().Purpose)
		return nil
	}
	if err := c.Log.Start(ctx); err != nil {
		return err
	}
	if c.supportsNewRestore(ctx) {
		return c.runRestore(ctx)
	}
	return c.runLegacyRestore(ctx)
}

func (c *restoreCommand) runRestore(ctx *cmd.Context) error {
	cmdArgs := []string{"backups"}
	if c.Log.Path != "" {
		cmdArgs = append(cmdArgs, "--log-file", c.Log.Path)
	}
	if c.Log.Verbose {
		cmdArgs = append(cmdArgs, "--verbose")
	}
	if c.Log.Quiet {
		cmdArgs = append(cmdArgs, "--quiet")
	}
	if c.Log.Debug {
		cmdArgs = append(cmdArgs, "--debug")
	}
	if c.Log.Config != c.Log.DefaultConfig {
		cmdArgs = append(cmdArgs, "--logging-config", c.Log.Config)
	}
	if c.Log.ShowLog {
		cmdArgs = append(cmdArgs, "--show-log")
	}

	cmdArgs = append(cmdArgs, "restore", "-b", "--file", c.backupFile)
	cmd := exec.Command("juju", cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (c *restoreCommand) supportsNewRestore(ctx *cmd.Context) bool {
	cmd := exec.Command("juju", "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logger.Errorf("cannot run juju version: %vr", err)
		return false
	}
	output := out.String()
	output = strings.TrimSpace(output)
	ver, err := version.ParseBinary(output)
	if err != nil {
		logger.Errorf("cannot parse juju version: %v", err)
		// if we cant parse the version number the version might
		// as well not be compatible.
		return false
	}
	// 1.25.0 is the minor version that will work certainly with
	// the new restore.
	restoreAvailableVersion := version.Number{
		Major: 1,
		Minor: 25,
		Patch: 0,
	}
	logger.Infof("current juju version is %q", output)
	return ver.Number.Compare(restoreAvailableVersion) >= 0
}
