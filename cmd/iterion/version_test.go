package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// stubVersionInfo pins the build metadata the command reads, so the
// populated-SHA path runs through the real cobra wiring. Without it that path
// is unreachable: a `go test` binary carries no VCS stamping and no ldflags.
func stubVersionInfo(t *testing.T, full, commit string) {
	t.Helper()
	prev := versionInfo
	versionInfo = func() (string, string) { return full, commit }
	t.Cleanup(func() { versionInfo = prev })
}

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
		// A plain `go build` infers the full 40-char vcs.revision, and the
		// container images inject a 40-char github.sha — both must agree with
		// the 12-char suffix `iterion version` prints.
		{
			name: "commit flag truncates a full SHA to the width version prints",
			full: "dev+e8adfa3f8823", commit: "e8adfa3f8823b262fbce5d2ed7ff9918d64b8591",
			commitOnly: true, want: "e8adfa3f8823",
		},
		// The Dockerfile defaults ARG COMMIT=unknown, so a bare `docker build .`
		// injects that sentinel rather than nothing.
		{name: "commit flag rejects the Dockerfile unknown sentinel", full: "dev+unknown", commit: "unknown", commitOnly: true, wantErr: true},
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

// TestVersionCommand_CommitFlag verifies the cobra wiring end to end on a
// build that DOES carry a SHA: `--commit` reaches versionOutput with the flag
// set, and the result is a single bare line with no version prefix.
func TestVersionCommand_CommitFlag(t *testing.T) {
	const sha = "e8adfa3f8823b262fbce5d2ed7ff9918d64b8591"
	stubVersionInfo(t, "v3.10.3+e8adfa3f8823", sha)

	got, err := runVersion(t, "version", "--commit")
	if err != nil {
		t.Fatalf("[version --commit] failed: %v", err)
	}
	if want := sha[:commitDisplayLen]; got != want {
		t.Errorf("commit output = %q, want %q", got, want)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("expected single line, got multi-line output: %q", got)
	}
	if strings.Contains(got, "v3.10.3") {
		t.Errorf("commit output leaked the version string: %q", got)
	}
}

// TestVersionCommand_CommitFlagNoSHA covers the other build shape through the
// same wiring: no injected SHA must fail rather than print an empty line.
func TestVersionCommand_CommitFlagNoSHA(t *testing.T) {
	stubVersionInfo(t, "dev", "")

	got, err := runVersion(t, "version", "--commit")
	if err == nil {
		t.Fatalf("expected an error with no commit SHA injected, got output %q", got)
	}
}

// TestVersionCommand_DefaultUnchanged guards the additive promise: with no
// flag, the command still prints the full human-readable version string of the
// real build.
func TestVersionCommand_DefaultUnchanged(t *testing.T) {
	got, err := runVersion(t, "version")
	if err != nil {
		t.Fatalf("[version] failed: %v", err)
	}
	if got != cli.Version() {
		t.Errorf("default output = %q, want %q", got, cli.Version())
	}
}
