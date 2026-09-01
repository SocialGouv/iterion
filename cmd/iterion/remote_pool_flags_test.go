package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// parsePoolPolicy drives the real flag registration over one command
// line. A fresh command per case is what keeps the cases independent:
// pflag assigns each default at bind time, so re-registering resets the
// package-level variables the flags are bound to.
func parsePoolPolicy(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "policy"}
	addPoolPolicyFlags(cmd)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cmd
}

// The Changed() gate is where "only what you set is sent" is actually
// decided — the pkg/cli tests exercise an already-built PoolPolicy one
// layer below it. Dropping one of these checks would restore exactly the
// forgotten---all-teams hazard the command exists to prevent, and every
// test below this layer would stay green.
func TestPoolPolicyFromFlags_SendsOnlyWhatWasSet(t *testing.T) {
	pol, err := poolPolicyFromFlags(parsePoolPolicy(t, "--enabled=false"))
	if err != nil {
		t.Fatalf("poolPolicyFromFlags: %v", err)
	}
	if pol.Enabled == nil || *pol.Enabled {
		t.Fatalf("Enabled = %v, want a pointer to false", pol.Enabled)
	}
	if pol.Audience != nil {
		t.Fatal("a pause must not restate the audience — that is how a forgotten --all-teams survives")
	}
	if pol.Name != nil {
		t.Fatalf("Name = %v, want absent", *pol.Name)
	}
}

// Bare `--enabled` still means true: cobra gives a bool flag
// NoOptDefVal="true", so registering the default as false (so --help
// stops promising one) changes the rendering only, never the value.
func TestPoolPolicyFromFlags_BareEnabledIsTrue(t *testing.T) {
	if got := remotePoolPolicyCmd.Flags().Lookup("enabled").DefValue; got != "false" {
		t.Errorf("--help would render `(default %s)`; omitting the flag leaves the pool unchanged, so it must not promise a value", got)
	}
	pol, err := poolPolicyFromFlags(parsePoolPolicy(t, "--enabled"))
	if err != nil {
		t.Fatalf("poolPolicyFromFlags: %v", err)
	}
	if pol.Enabled == nil || !*pol.Enabled {
		t.Fatalf("Enabled = %v, want a pointer to true", pol.Enabled)
	}
}

// Naming any audience dial sends the audience WHOLE: it is a set, so the
// dials the caller left out travel as their zero value rather than being
// merged server-side with whatever was there before.
func TestPoolPolicyFromFlags_AudienceTravelsWhole(t *testing.T) {
	pol, err := poolPolicyFromFlags(parsePoolPolicy(t, "--teams", "team-a,team-b"))
	if err != nil {
		t.Fatalf("poolPolicyFromFlags: %v", err)
	}
	if pol.Audience == nil {
		t.Fatal("naming --teams must send an audience")
	}
	if len(pol.Audience.Teams) != 2 || pol.Audience.Teams[0] != "team-a" {
		t.Fatalf("Teams = %v, want [team-a team-b]", pol.Audience.Teams)
	}
	if pol.Audience.AllTeams || pol.Audience.Contributors || len(pol.Audience.Orgs) != 0 {
		t.Fatalf("unnamed dials must travel as zero, got %+v", *pol.Audience)
	}
	if pol.Enabled != nil {
		t.Fatal("an audience edit must not restate the master switch")
	}
}

// A fresh command must not inherit the previous one's values — the
// property the per-case registration relies on. Without it a later case
// asserting "unnamed dials are zero" would pass on stale data.
func TestPoolPolicyFromFlags_CasesDoNotLeak(t *testing.T) {
	if _, err := poolPolicyFromFlags(parsePoolPolicy(t, "--teams", "leaked", "--all-teams")); err != nil {
		t.Fatalf("poolPolicyFromFlags: %v", err)
	}
	pol, err := poolPolicyFromFlags(parsePoolPolicy(t, "--contributors"))
	if err != nil {
		t.Fatalf("poolPolicyFromFlags: %v", err)
	}
	if len(pol.Audience.Teams) != 0 || pol.Audience.AllTeams {
		t.Fatalf("previous case leaked into this one: %+v", *pol.Audience)
	}
}

// Naming nothing is refused rather than sent: an empty PUT body would
// read as a successful no-op and tell the operator nothing.
func TestPoolPolicyFromFlags_RefusesAnEmptyEdit(t *testing.T) {
	_, err := poolPolicyFromFlags(parsePoolPolicy(t))
	if err == nil {
		t.Fatal("want a refusal when no flag was set")
	}
	if !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("unexpected error: %v", err)
	}
}
