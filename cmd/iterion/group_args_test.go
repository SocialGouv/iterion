package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// groupsAcceptingAPositional names the groups whose own Args deliberately
// admits a positional, so the unknown-subcommand rejection has to come from
// the command itself rather than from Args. Each needs its RunE driven to
// prove it — and RunE is only safe to drive in-process for a command with no
// side effects, which is the judgment this map records.
//
// `iterion models [provider/model-id]` lists every model when bare, inspects
// one when given a spec, and `models pricing` made it a group as well. Its
// RunE validates the spec before touching anything.
var groupsAcceptingAPositional = map[string]bool{
	"iterion models": true,
}

// TestRejectUnknownSubcommands_CoversEveryGroup walks the real command
// tree and asserts no group can answer a typo with help and exit 0.
//
// The regression this guards is silent: cobra returns ErrHelp for a
// non-Runnable command BEFORE validating args, so a group left untouched
// answers `iterion <group> <typo>` by printing help and exiting 0 — which
// reads as success. A group added later without going through
// rejectUnknownSubcommands would reintroduce exactly that.
//
// Every group must be Runnable (else cobra never consults Args at all) and
// must turn an unknown positional into an error. Where that error comes from
// is allowed to differ: `Args` for the help-only groups the sweep hardens and
// for the runnable ones already declaring cobra.NoArgs, the command's own
// validation for a group that legitimately takes a positional. What is NOT
// allowed is for a group to accept the argument and act on it.
func TestRejectUnknownSubcommands_CoversEveryGroup(t *testing.T) {
	rejectUnknownSubcommands(rootCmd)

	const typo = "definitely-not-a-subcommand"
	var groups, viaArgs []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c == rootCmd || !c.HasSubCommands() {
			return
		}
		path := c.CommandPath()
		groups = append(groups, path)

		// A non-Runnable group short-circuits to help before Args is ever
		// consulted, whatever Args says.
		if !c.Runnable() {
			t.Errorf("%q is not Runnable, so cobra short-circuits to help before Args runs", path)
		}
		if c.Args == nil {
			t.Errorf("%q has subcommands but no Args validation: a typo would exit 0", path)
			return
		}
		// A bare invocation must still be allowed — it prints help (or, for a
		// group that does something itself, does it).
		if err := c.Args(c, nil); err != nil {
			t.Errorf("%q rejects a bare invocation (%v); it should not", path, err)
		}

		if err := c.Args(c, []string{typo}); err != nil {
			viaArgs = append(viaArgs, path)
			return
		}
		// Args let the typo through, so the command itself is the only thing
		// left that can reject it. Prove it does.
		if !groupsAcceptingAPositional[path] {
			t.Errorf("%q accepts an unknown subcommand through Args and is not a known "+
				"positional-taking group — either give it cobra.NoArgs, or add it to "+
				"groupsAcceptingAPositional once you have confirmed its RunE rejects a typo "+
				"and is safe to drive in-process", path)
			return
		}
		if c.RunE == nil {
			t.Errorf("%q takes a positional but has no RunE to validate it", path)
			return
		}
		if err := c.RunE(c, []string{typo}); err == nil {
			t.Errorf("%q ran with %q and reported success — a typo must not be silently accepted", path, typo)
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
	// And the rejection is broad rather than concentrated in the exceptions.
	if len(viaArgs) < 20 {
		t.Errorf("only %d of %d groups reject a typo at the Args level (%s)",
			len(viaArgs), len(groups), strings.Join(viaArgs, ", "))
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
