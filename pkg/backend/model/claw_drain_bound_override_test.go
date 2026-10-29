package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// The crossing's drain bound is the launcher's promise that a dispatch hands
// the node back; what it COSTS is the tail of the container's log — the
// stderr a descendant of the exited runner is still writing. #2469: that
// trade was compiled in with no way out, so an operator debugging a container
// whose last words are the ones that matter had no answer. The one override
// is the answer, and this is the crossing — the widest of the nine bounds —
// honouring it.
//
// Both directions in one shape: the site's own 2 s abandons the descendant's
// last line and SAYS so, naming the knob; the operator's hour keeps it.
//
// Mutations seen red (M13): `cmd.WaitDelay = clawRunnerWaitDelay` instead of
// proc.SetWaitDelay — the override subtest loses the last word; and reporting
// `clawRunnerWaitDelay` instead of the applied bound — the report names a
// bound that never ran.
func TestSandboxDispatch_TheOperatorsDrainBoundKeepsTheRunnersLastWord(t *testing.T) {
	const lastWord = "the-containers-last-word"
	// Derived from the site's own bound, so shortening the default cannot
	// make this vacuous: the descendant writes AFTER the default would have
	// abandoned the drain.
	late := clawRunnerWaitDelay + 500*time.Millisecond

	for _, tc := range []struct {
		name         string
		override     string
		wantLastWord bool
	}{
		{name: "the site's own bound abandons the drain", override: "", wantLastWord: false},
		{name: "the operator's hour keeps the last word", override: "1h", wantLastWord: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(proc.WaitDelayEnv, tc.override)
			dir := t.TempDir()
			resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
			// `trap '' PIPE`: past the bound the pipes are closed under the
			// descendant, and the default SIGPIPE would kill it before it
			// could report that it had finished — which the test joins, so
			// no shell outlives the suite.
			done := filepath.Join(dir, "holder-exited")
			script := fmt.Sprintf("( trap '' PIPE; sleep %.1f; printf '%%s\\n' %q >&2; : > %q ) &\n",
				late.Seconds(), lastWord, done) + awaitTaskEnvelope + "cat \"$1\"\nexit 0"
			t.Cleanup(func() { awaitHolderExit(t, done) })

			var logs strings.Builder
			b := loggedTestClawBackend(&logs)
			res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
				NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
				Sandbox: &scriptedSandbox{script: script, args: []string{resultFile}},
			})
			if err != nil {
				t.Fatalf("the crossing failed: %v", err)
			}
			if res.Output["verdict"] != "ok" {
				t.Fatalf("output = %v, want the runner's answer", res.Output)
			}

			out := logs.String()
			if got := strings.Contains(out, lastWord); got != tc.wantLastWord {
				t.Errorf("the descendant's last word reached the operator's log = %v, want %v: %q", got, tc.wantLastWord, out)
			}
			switch tc.wantLastWord {
			case true:
				// A drain that was waited for is not an abandoned one.
				if strings.Contains(out, "abandoned the drain") {
					t.Errorf("the hour-long bound still abandoned the drain: %q", out)
				}
			default:
				// The cut is reported, and the report names the bound that
				// RAN and the variable that widens it — the escape hatch is
				// greppable from the log that shows the loss.
				if !strings.Contains(out, "abandoned the drain after "+clawRunnerWaitDelay.String()) {
					t.Errorf("the abandoned drain is unreported, or names another bound: %q", out)
				}
				if !strings.Contains(out, proc.WaitDelayEnv) {
					t.Errorf("the report does not name the knob that widens the bound: %q", out)
				}
			}
		})
	}
}

// awaitHolderExit joins the descendant the scripted runner left behind: it
// writes done as its last act, and a test that returned without it would
// leave a shell running past the suite.
func awaitHolderExit(t *testing.T, done string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the runner's descendant never reported its exit (%s) — it may outlive the suite", done)
}
