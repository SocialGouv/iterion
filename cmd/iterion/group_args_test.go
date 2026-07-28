package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRejectUnknownSubcommands_CoversEveryGroup walks the real command
// tree and asserts no group can answer a typo with help and exit 0.
//
// The regression this guards is silent: cobra returns ErrHelp for a
// non-Runnable command BEFORE validating args, so a group left untouched
// answers `iterion <group> <typo>` by printing help and exiting 0 — which
// reads as success. A group added later without going through
// rejectUnknownSubcommands would reintroduce exactly that.
//
// Two shapes clear that bar, so each group is held to the one it has:
//   - a help-only group (stamped by the sweep) must be Runnable and reject
//     any argument, since it has no meaning for one;
//   - a group that ALSO takes positional args of its own (`iterion models
//     [provider/model-id]`) must be Runnable with its own non-nil Args —
//     cobra then executes it and the command's own validation answers.
//     Nothing may sit outside both shapes with Args left nil.
func TestRejectUnknownSubcommands_CoversEveryGroup(t *testing.T) {
	rejectUnknownSubcommands(rootCmd)

	var groups, helpOnly []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c == rootCmd || !c.HasSubCommands() {
			return
		}
		groups = append(groups, c.CommandPath())

		// Runnable is the shared half: a non-Runnable group short-circuits
		// to help before Args is ever consulted, whatever Args says.
		if !c.Runnable() {
			t.Errorf("%q is not Runnable, so cobra short-circuits to help before Args runs",
				c.CommandPath())
		}
		if c.Args == nil {
			t.Errorf("%q has subcommands but no Args validation: a typo would exit 0", c.CommandPath())
			return
		}
		if c.Annotations[groupHelpOnlyAnnotation] != "true" {
			// Its own positional contract — the sweep correctly left it
			// alone, and the argument reaches its own validation.
			return
		}
		helpOnly = append(helpOnly, c.CommandPath())
		if err := c.Args(c, []string{"definitely-not-a-subcommand"}); err == nil {
			t.Errorf("%q accepts an unknown subcommand instead of rejecting it", c.CommandPath())
		}
		// A bare invocation must still be allowed — it prints help.
		if err := c.Args(c, nil); err != nil {
			t.Errorf("%q rejects a bare invocation (%v); it should print help", c.CommandPath(), err)
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
	// And the sweep did the hardening, rather than every group happening to
	// carry its own Args — which would make the assertions above vacuous.
	if len(helpOnly) < 20 {
		t.Errorf("only %d of %d groups were hardened by the sweep (%s) — the guard is barely doing anything",
			len(helpOnly), len(groups), strings.Join(helpOnly, ", "))
	}
}

// TestRejectUnknownSubcommands_PinsTheSelfDeclaredGroups pins the groups
// the sweep deliberately leaves alone — the ones that host subcommands AND
// do something themselves. A new hybrid appearing unnoticed, or a group
// silently losing its Args, shows up here as a diff rather than as coverage
// quietly shrinking.
func TestRejectUnknownSubcommands_PinsTheSelfDeclaredGroups(t *testing.T) {
	rejectUnknownSubcommands(rootCmd)

	var own []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c == rootCmd || !c.HasSubCommands() {
			return
		}
		if c.Annotations[groupHelpOnlyAnnotation] != "true" {
			own = append(own, c.CommandPath())
		}
	}
	walk(rootCmd)

	// Each one rejects a typo through its own contract, verified by hand:
	//   iterion models <typo>    → exit 2 (its own spec validation:
	//                              `[provider/model-id]` inspects one model,
	//                              bare lists them all, `pricing` made it a
	//                              group as well)
	//   iterion server <typo>    → exit 1 (runnable + cobra.NoArgs)
	//   iterion supervise <typo> → exit 1 (runnable + cobra.NoArgs)
	want := "iterion models, iterion server, iterion supervise"
	if got := strings.Join(own, ", "); got != want {
		t.Errorf("groups declaring their own argument contract = %q, want %q\n"+
			"a new one is fine — confirm `iterion <group> <typo>` exits non-zero, then update this list",
			got, want)
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
