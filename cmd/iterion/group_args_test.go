package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRejectUnknownSubcommands_CoversEveryGroup walks the real command
// tree and asserts every group command ends up rejecting arguments.
//
// The regression this guards is silent: cobra returns ErrHelp for a
// non-Runnable command BEFORE validating args, so a group left untouched
// answers `iterion <group> <typo>` by printing help and exiting 0 — which
// reads as success. A group added later without going through
// rejectUnknownSubcommands would reintroduce exactly that.
func TestRejectUnknownSubcommands_CoversEveryGroup(t *testing.T) {
	rejectUnknownSubcommands(rootCmd)

	var groups []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c == rootCmd || !c.HasSubCommands() {
			return
		}
		groups = append(groups, c.CommandPath())
		if c.Args == nil {
			t.Errorf("%q has subcommands but no Args validation: a typo would exit 0", c.CommandPath())
			return
		}
		if err := c.Args(c, []string{"definitely-not-a-subcommand"}); err == nil {
			t.Errorf("%q accepts an unknown subcommand instead of rejecting it", c.CommandPath())
		}
		// A bare invocation must still be allowed — it prints help.
		if err := c.Args(c, nil); err != nil {
			t.Errorf("%q rejects a bare invocation (%v); it should print help", c.CommandPath(), err)
		}
		if !c.Runnable() {
			t.Errorf("%q is not Runnable, so cobra short-circuits to help before Args runs",
				c.CommandPath())
		}
	}
	walk(rootCmd)

	// Sanity: the walk actually found the tree, so a future refactor that
	// silently stops registering commands fails here rather than passing
	// vacuously.
	if len(groups) < 20 {
		t.Fatalf("only found %d group commands (%s) — expected the full tree",
			len(groups), strings.Join(groups, ", "))
	}
}

// TestRejectUnknownSubcommands_LeavesLeavesAlone makes sure the sweep
// does not clobber a real command's own argument contract.
func TestRejectUnknownSubcommands_LeavesLeafCommandsAlone(t *testing.T) {
	rejectUnknownSubcommands(rootCmd)

	// `bots create <slug>` takes exactly one positional arg; the sweep
	// must not have replaced that with NoArgs.
	create, _, err := rootCmd.Find([]string{"bots", "create"})
	if err != nil {
		t.Fatalf("Find(bots create): %v", err)
	}
	if err := create.Args(create, []string{"my-bot"}); err != nil {
		t.Errorf("bots create rejects its slug argument: %v", err)
	}
}
