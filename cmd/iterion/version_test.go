package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// runVersion drives the real rootCmd (the only correct way — versionCmd has a
// parent, so cobra's Execute delegates to root and ignores args set directly on
// the subcommand) and returns its combined stdout/stderr.
func runVersion(t *testing.T, args ...string) string {
	t.Helper()
	t.Cleanup(func() {
		versionCommitOnly = false
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("%v failed: %v", args, err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// TestVersionCommand_CommitFlag verifies that `iterion version --commit`
// prints ONLY the bare commit SHA on a single line — no version prefix,
// no extra text — so scripts can capture the SHA directly.
func TestVersionCommand_CommitFlag(t *testing.T) {
	got := runVersion(t, "version", "--commit")

	if got != cli.RawCommit() {
		t.Errorf("commit output = %q, want %q", got, cli.RawCommit())
	}
	if strings.Contains(got, "\n") {
		t.Errorf("expected single line, got multi-line output: %q", got)
	}
	if cli.RawCommit() != cli.Version() && strings.Contains(got, cli.Version()) {
		t.Errorf("commit output leaked full version string: %q", got)
	}
}

// TestVersionCommand_DefaultUnchanged guards the additive promise: with no
// flag, the command still prints the full human-readable version string.
func TestVersionCommand_DefaultUnchanged(t *testing.T) {
	got := runVersion(t, "version")

	if got != cli.Version() {
		t.Errorf("default output = %q, want %q", got, cli.Version())
	}
}
