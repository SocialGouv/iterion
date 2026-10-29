package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// Where the OUTPUT is the answer, a cut drain is not a success. git exits 0
// while a helper it left (git-remote-https, a credential helper) still holds
// the pipes CombinedOutput reads; what is in hand at the bound may be half a
// sha, and the callers of this chokepoint parse it (rev-parse, ls-remote,
// rev-list). So the cut stays an error — one that NAMES the bound that ran
// and the variable that widens it, because the fix is an operator's and no
// retry can find it.
//
// The shim is a `git` on PATH: iterion resolves the binary from the
// environment, and the shape under test is the helper's, not git's.
//
// Mutation seen red: replace the proc.DrainCutShort(err) arm in loop_gitws.go
// with `if false` — the error comes back as the bare sentinel, naming neither
// the bound nor the knob (M14).
func TestRunGitOut_ACutDrainNamesTheBoundAndTheKnob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pipe-holding shim is a POSIX shell script")
	}
	dir := t.TempDir()
	hold := filepath.Join(dir, "hold")
	done := filepath.Join(dir, "holder-exited")
	if err := os.WriteFile(hold, []byte("held"), 0o600); err != nil {
		t.Fatalf("arm the pipe holder: %v", err)
	}
	// Written by the HELPER after git is gone; the bound cuts the read
	// before it lands, which is the truncation this error exists to refuse.
	shim := fmt.Sprintf(
		"#!/bin/sh\n( trap '' PIPE; while [ -e %q ]; do sleep 0.05; done; printf 'the-tail-of-the-answer\\n' 2>/dev/null; : > %q ) &\nprintf 'deadbeef\\n'\nexit 0\n",
		hold, done)
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(proc.WaitDelayEnv, "150ms")
	// The helper outlives the command until the test lets it go, and the
	// test JOINS it: a shell left behind would outlive the suite, which this
	// package's leak guard fails.
	t.Cleanup(func() {
		if err := os.Remove(hold); err != nil && !os.IsNotExist(err) {
			t.Fatalf("release the pipe holder: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(done); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("the pipe holder did not exit after its sentinel went away — it would outlive the suite")
	})

	start := time.Now()
	out, err := (&Runner{}).runGitOutEnv(context.Background(), dir, "", nil, "rev-parse", "HEAD")
	elapsed := time.Since(start)

	if _, serr := os.Stat(done); serr == nil {
		t.Fatalf("the helper was gone before the bound: this run was not the held shape the witness needs")
	}
	if err == nil {
		t.Fatalf("a read cut mid-answer came back as the answer: %q", out)
	}
	if out != "" {
		t.Errorf("the truncated output was handed to the caller as well: %q", out)
	}
	for _, want := range []string{proc.WaitDelayEnv, "150ms", "rev-parse HEAD", "deadbeef"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q — an operator cannot act on it: %v", want, err)
		}
	}
	// The operator's bound is the one that ran: this site's own default is
	// 10 s, so returning inside a second is only possible through it.
	if elapsed >= gitDrainBound/2 {
		t.Fatalf("the command came back after %s with %s=150ms set — this site's compiled %s bound is what ran",
			elapsed, proc.WaitDelayEnv, gitDrainBound)
	}
}
