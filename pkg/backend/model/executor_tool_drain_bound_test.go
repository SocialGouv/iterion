package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// A tool node's command is the BOT's text, and backgrounding a process in it
// is an authored recipe: the shell exits, the job it forked inherits the
// output pipe, and [exec.Cmd.Wait] ends only when EVERY holder has closed
// it. The node's own timeout kills the shell, never what the shell forked,
// so with no bound on the drain the node is parked for as long as that job
// lives (#2855, the class of #2469).
//
// Both directions in one shape: the site's own bound cuts the read and SAYS
// so, naming the knob, because what the node answers with is those bytes;
// the operator's hour keeps them.
//
// Mutations seen red: drop the proc.SetWaitDelay call in
// runWithSeparateStreams — the bounded subtests' node does not come back
// within this test's deadline; and report toolNodeDrainBound instead of the
// applied bound — the tightened-override subtest reads a bound that never
// ran.
func TestToolNodeCommandReturnsWhileALeftoverHoldsItsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	const answer = "the-nodes-answer"
	// Derived from the site's own bound, so shortening the default cannot
	// make this vacuous: the holder outlives the bound that must cut it. The
	// margin is wall-clock slack on a loaded machine, where the bound's timer
	// can fire late — a holder that lets go as the bound fires would make the
	// verdict a race between a timer and a sleep.
	holds := toolNodeDrainBound + 2*time.Second

	for _, tc := range []struct {
		name string
		// override is ITERION_PROC_WAIT_DELAY for the subtest; "" is unset,
		// which is how the site's own default gets exercised.
		override string
		// wantBound is the bound the report must name — the one that RAN,
		// never this site's default, or a tightened override reads as a loss
		// at a bound nobody set.
		wantBound  string
		wantCutOff bool
	}{
		{name: "the site's own bound cuts the read and reports it", override: "", wantBound: toolNodeDrainBound.String(), wantCutOff: true},
		{name: "a tightened override is the bound the report names", override: "500ms", wantBound: "500ms", wantCutOff: true},
		{name: "the operator's hour waits the holder out", override: "1h", wantCutOff: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(proc.WaitDelayEnv, tc.override)
			// `trap '' PIPE`: past the bound the pipes are closed under the
			// holder, and the default SIGPIPE would kill it before it could
			// report that it had finished — which the test joins, so no
			// shell outlives the suite.
			done := filepath.Join(t.TempDir(), "holder-exited")
			body := fmt.Sprintf("( trap '' PIPE; sleep %.1f; : > %q ) &\nprintf '%%s\\n' %q\nexit 0",
				holds.Seconds(), done, answer)
			t.Cleanup(func() { awaitToolNodeHolderExit(t, done) })

			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: body}
			ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()))

			// The wait is MEASURED, never waited out: a regression that
			// parks the node has to fail here in seconds instead of holding
			// the package to its timeout.
			type outcome struct {
				out map[string]any
				err error
			}
			got := make(chan outcome, 1)
			go func() {
				out, err := ex.Execute(context.Background(), node, map[string]any{})
				got <- outcome{out: out, err: err}
			}()
			var res outcome
			select {
			case res = <-got:
			case <-time.After(30 * time.Second):
				t.Fatalf("the tool node did not come back in 30s: it waits for the output a process its command left holds")
			}

			if !tc.wantCutOff {
				if res.err != nil {
					t.Fatalf("the operator's hour-long bound still failed the node: %v", res.err)
				}
				return
			}
			if res.err == nil {
				t.Fatalf("a node whose output was read short succeeded (%v)", res.out)
			}
			// The cut is reported, and the report names the bound that RAN
			// and the variable that widens it — the escape hatch is
			// greppable from the error that shows the loss.
			for _, want := range []string{"read short", tc.wantBound, proc.WaitDelayEnv} {
				if !strings.Contains(res.err.Error(), want) {
					t.Errorf("the error does not name %q: %v", want, res.err)
				}
			}
			if other := toolNodeDrainBound.String(); tc.wantBound != other && strings.Contains(res.err.Error(), other) {
				t.Errorf("the error names this site's default %q instead of the bound that ran %q: %v", other, tc.wantBound, res.err)
			}
		})
	}
}

// awaitToolNodeHolderExit joins the process the node's command left behind:
// it writes done as its last act, and a test that returned without it would
// leave a shell running past the suite.
func awaitToolNodeHolderExit(t *testing.T, done string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the command's leftover process never reported its exit (%s) — it may outlive the suite", done)
}
