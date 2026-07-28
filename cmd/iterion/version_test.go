package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// runVersion drives the real rootCmd (the only correct way — versionCmd has a
// parent, so cobra's Execute delegates to root and ignores args set directly on
// the subcommand) and returns its combined stdout/stderr plus the exec error.
func runVersion(t *testing.T, args ...string) (string, error) {
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
	err := rootCmd.Execute()
	return strings.TrimRight(buf.String(), "\n"), err
}

// TestVersionOutput covers both build shapes directly. A test binary carries
// no injected SHA, so driving the command end to end can only ever exercise
// the empty-commit branch — hence the pure function.
func TestVersionOutput(t *testing.T) {
	tests := []struct {
		name       string
		full       string
		commit     string
		commitOnly bool
		want       string
		wantErr    bool
	}{
		{name: "default prints the full version", full: "v1.2.3+abc1234", commit: "abc1234", want: "v1.2.3+abc1234"},
		{name: "commit flag prints only the SHA", full: "v1.2.3+abc1234", commit: "abc1234", commitOnly: true, want: "abc1234"},
		{name: "commit flag trims surrounding space", full: "v1.2.3", commit: "  abc1234\n", commitOnly: true, want: "abc1234"},
		{name: "commit flag errors when no SHA was injected", full: "dev", commit: "", commitOnly: true, wantErr: true},
		{name: "default is unaffected by a missing SHA", full: "dev", commit: "", want: "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := versionOutput(tt.full, tt.commit, tt.commitOnly)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got output %q", got)
				}
				if got != "" {
					t.Errorf("expected no output alongside the error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVersionCommand_CommitFlag verifies the cobra wiring: `--commit` reaches
// versionOutput with the flag set, and the result is printed on a single line
// with no version prefix. A test binary usually has no SHA, in which case the
// command must fail rather than print an empty line.
func TestVersionCommand_CommitFlag(t *testing.T) {
	got, err := runVersion(t, "version", "--commit")

	if strings.TrimSpace(cli.RawCommit()) == "" {
		if err == nil {
			t.Fatalf("expected an error with no commit SHA injected, got output %q", got)
		}
		return
	}
	if err != nil {
		t.Fatalf("[version --commit] failed: %v", err)
	}
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
	got, err := runVersion(t, "version")
	if err != nil {
		t.Fatalf("[version] failed: %v", err)
	}
	if got != cli.Version() {
		t.Errorf("default output = %q, want %q", got, cli.Version())
	}
}
