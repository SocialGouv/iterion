package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A tool node's command is the BOT's text, and backgrounding a process in it
// is an authored recipe: the shell exits, the job it forked inherits the
// output pipe, and [exec.Cmd.Wait] ends only when EVERY holder has closed
// it. With no bound the node is parked for as long as that job lives
// (#2855, the class of #2469).
//
// The node's verdict is its command's EXIT STATUS — the status is what fails
// it, and the Verified Action ladder reads the same runs as an oracle
// (ADR-044) — so the drain bound's doctrine gives it the exit-status
// reading: the success and the answer are KEPT, and the abandoned drain is
// REPORTED, on the node's event and in the run log. A recipe that worked and
// left a server running is warned about, never failed (philosophy §1), and
// the report names the bound that RAN and the knob that widens it.
//
// Mutations seen red: clear the cut in [ClawExecutor.runToolNodeCore] without
// reporting it (the event and the run log say nothing of the drain the bound
// abandoned); report [toolNodeDrainBound] instead of the bound that ran (the
// tightened-override arm reads a bound nobody set); drop the
// [proc.SetWaitDelay] call in runWithSeparateStreams (the bounded arms do not
// come back within this test's deadline); and keep the sentinel as the run's
// error (the node fails on a complete answer, and its deliverables are never
// handed over).
func TestToolNodeKeepsItsAnswerAndReportsTheDrainALeftoverHeld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	const answer = "the-nodes-answer"

	for _, tc := range []struct {
		name string
		// override is ITERION_PROC_WAIT_DELAY for the subtest; "" is unset,
		// which is how the site's own default gets exercised.
		override string
		// wantBound is the bound the report must name — the one that RAN,
		// never this site's default, or a tightened override reads as a
		// drain abandoned at a bound nobody set.
		wantBound string
		// wantReported is whether the bound bit at all: the operator who
		// asked to wait gets the whole drain, and nothing to report.
		wantReported bool
	}{
		{name: "the site's own bound stops the wait and says so", override: "", wantBound: toolNodeDrainBound.String(), wantReported: true},
		{name: "a tightened override is the bound the report names", override: "500ms", wantBound: "500ms", wantReported: true},
		{name: "the operator's hour waits the holder out and reports nothing", override: "1h", wantReported: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(proc.WaitDelayEnv, tc.override)
			holders := newLeftoverHolders(t)
			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: holders.recipe(answer)}

			var logged lockedSink
			events := &toolNodeEvents{}
			ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
				WithToolPolicy(tool.OpenPolicy()),
				WithLogger(iterlog.New(iterlog.LevelInfo, &logged)),
				events.hooks())

			// The wait is MEASURED, never waited out: a regression that
			// parks the node has to fail here in seconds instead of holding
			// the package to its timeout.
			type outcome struct {
				out map[string]any
				err error
			}
			got := make(chan outcome, 1)
			start := time.Now()
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
			elapsed := time.Since(start)
			t.Cleanup(func() { holders.join(1) })

			if res.err != nil {
				t.Fatalf("the node failed although its command exited 0 and printed its whole answer: %v", res.err)
			}
			if res.out["result"] != answer {
				t.Fatalf("the node's output is %v, want the answer its command printed (%q)", res.out, answer)
			}
			// A recipe that SUCCEEDED is held to what it declared: the
			// delivery of the files its output names runs only on the
			// success this reading keeps.
			if !events.delivered() {
				t.Error("the node's deliverables were never handed over, so a `required=true` file could not be missed")
			}
			if err := events.callError(); err != nil {
				t.Errorf("the node's event carries an error on a command that exited 0: %v", err)
			}

			report := events.callOutput() + "\n" + logged.String()
			if !tc.wantReported {
				if elapsed < holders.holds {
					t.Errorf("the node came back in %s, before the holder let go (%s): the operator's bound did not wait it out", elapsed, holders.holds)
				}
				if strings.Contains(report, proc.WaitDelayEnv) {
					t.Errorf("a drain nothing cut short is reported anyway: %s", report)
				}
				return
			}
			if elapsed >= holders.holds {
				t.Errorf("the node came back in %s, no sooner than the holder's own %s: the bound did not stop the wait", elapsed, holders.holds)
			}
			// What the report must say is what happened: the command
			// exited 0, something it left still holds the output, and the
			// wait stopped at the bound that ran. "read short" would be a
			// fact this site does not have — the answer above is whole.
			for _, want := range []string{"exited 0", "still holds its output", "stopped waiting", tc.wantBound, proc.WaitDelayEnv} {
				if !strings.Contains(events.callOutput(), want) {
					t.Errorf("the node's event does not name %q: %s", want, events.callOutput())
				}
				if !strings.Contains(logged.String(), want) {
					t.Errorf("the run log does not name %q: %s", want, logged.String())
				}
			}
			if strings.Contains(report, "read short") {
				t.Errorf("the report claims the output was read short, which this site cannot know and the answer above disproves: %s", report)
			}
			if other := toolNodeDrainBound.String(); tc.wantBound != other && strings.Contains(report, other) {
				t.Errorf("the report names this site's default %q instead of the bound that ran %q: %s", other, tc.wantBound, report)
			}
		})
	}
}

// A postcondition is an EXIT-STATUS oracle: ADR-044 makes "exit 0 = met" the
// single source of truth of a Verified Action, which is what keeps the
// recovery rungs safe. So by the drain bound's own doctrine it belongs to the
// family that keeps the success and REPORTS the abandoned drain (#2855, the
// class of #2469): a check that exits 0 while a process it left holds its
// output is MET.
//
// What the other reading costs is measured here, not argued: the oracle
// answers UNMET, rung 1 does not skip, and the recipe — a node whose whole
// purpose is not to repeat its side effect — runs a second time.
//
// Mutations seen red: make runToolNodeCore keep the sentinel as the run's
// error (the oracle reads a check that exited 0 as unmet, and the marker
// below proves the side effect ran); and drop the proc.SetWaitDelay call in
// runWithSeparateStreams (neither subtest comes back within its deadline).
func TestAPostconditionThatExitedZeroIsMetWhileALeftoverHoldsItsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	// The bound asserted below is this site's, so the environment the suite
	// runs in does not get to choose it.
	t.Setenv(proc.WaitDelayEnv, "")

	t.Run("the oracle reads the exit status, so the check is met", func(t *testing.T) {
		holders := newLeftoverHolders(t)
		node := &ir.ToolNode{
			BaseNode:      ir.BaseNode{ID: "gate"},
			Command:       "echo the-recipe-should-not-be-reached",
			Postcondition: holders.recipe(`{"state":"already-there"}`),
		}
		ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()))

		type verdict struct {
			met bool
			out map[string]any
			err error
		}
		got := make(chan verdict, 1)
		go func() {
			met, out, err := ex.runPostcondition(context.Background(), node, map[string]any{})
			got <- verdict{met: met, out: out, err: err}
		}()
		var res verdict
		select {
		case res = <-got:
		case <-time.After(30 * time.Second):
			t.Fatalf("the postcondition did not come back in 30s: it waits for the output a process it left holds")
		}
		t.Cleanup(func() { holders.join(1) })

		if res.err != nil {
			t.Fatalf("the postcondition errored although it exited 0: %v", res.err)
		}
		if !res.met {
			t.Fatalf("a postcondition that exited 0 is read as UNMET because a process it left holds its output (output %v)", res.out)
		}
		// The state the check printed is what the skip rung publishes, so a
		// cut that kept the success must keep those bytes too.
		if res.out["state"] != "already-there" {
			t.Errorf("the postcondition's output is %v, want the state it printed", res.out)
		}
	})

	t.Run("rung 1 skips, so the side effect does not run a second time", func(t *testing.T) {
		holders := newLeftoverHolders(t)
		dir := t.TempDir()
		marker := filepath.Join(dir, "the-side-effect-ran")
		node := &ir.ToolNode{
			BaseNode:      ir.BaseNode{ID: "deploy"},
			Command:       ": > " + marker + "\nprintf '%s\\n' '{\"did\":\"the-side-effect\"}'",
			Postcondition: holders.recipe(`{"state":"already-there"}`),
			Goal:          "the state is already there",
		}
		events := &toolNodeEvents{}
		ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
			WithToolPolicy(tool.OpenPolicy()), events.hooks())

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
			t.Fatalf("the verified node did not come back in 30s: it waits for the output a process its postcondition left holds")
		}
		// Each run of the postcondition left one holder; the ladder's own
		// events say how many ran, and all of them are joined.
		t.Cleanup(func() { holders.join(events.countCalls(postcondToolName(node))) })

		if res.err != nil {
			t.Fatalf("the node failed although its postcondition exited 0: %v", res.err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("the recipe RAN although the postcondition was already met: the idempotent-skip rung was defeated and the side effect happened a second time")
		}
		if rung := vaMeta(t, res.out)["rung"]; rung != "idempotent_skip" {
			t.Errorf("the node ended on rung %v, want idempotent_skip", rung)
		}
	})
}

// A layer below can have chosen the bound already — a sandbox driver that
// bounded the client it built (the kubernetes one, for a credential plugin
// holding kubectl's pipes, through this same override) — and the shared
// runner KEEPS that choice: it takes this site's default only for a Cmd that
// carries none. A report naming this site's default instead would send an
// operator to raise a bound that never ran.
//
// The runner also hands exec's sentinel back UNWRAPPED, because the reading
// is the caller's: the message is built where the family that reads it is
// known, never in the runner four families share.
//
// Mutations seen red: replace the `if drainBound == 0` test with an
// unconditional proc.SetWaitDelay (the bound comes back as this site's own
// default, not the one the layer below chose); and wrap the sentinel in the
// runner again (the unwrapped assertion reads iterion prose where exec's own
// error belongs).
func TestTheSharedRunnerKeepsABoundChosenBelowItAndHandsTheCutBackUnwrapped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	// The bound asserted below is the layer's, so the environment the suite
	// runs in does not get to choose it.
	t.Setenv(proc.WaitDelayEnv, "")
	// Recognisable, and not this site's own default: a report naming 2s
	// after this would be naming a bound nobody set.
	const chosenBelow = 750 * time.Millisecond
	const answer = "the-commands-answer"

	holders := newLeftoverHolders(t)
	cmd := exec.Command("bash", "-c", holders.recipe(answer))
	cmd.WaitDelay = chosenBelow

	type run struct {
		stdout []byte
		bound  time.Duration
		err    error
	}
	got := make(chan run, 1)
	start := time.Now()
	go func() {
		stdout, _, bound, err := runWithSeparateStreams(cmd)
		got <- run{stdout: stdout, bound: bound, err: err}
	}()
	var res run
	select {
	case res = <-got:
	case <-time.After(30 * time.Second):
		t.Fatalf("the run did not come back in 30s: it waits for the output a process the command left holds")
	}
	elapsed := time.Since(start)
	t.Cleanup(func() { holders.join(1) })

	if res.bound != chosenBelow {
		t.Errorf("the runner applied %s over the %s a layer below it had chosen", res.bound, chosenBelow)
	}
	if elapsed >= holders.holds {
		t.Errorf("the run came back in %s, no sooner than the holder's own %s: the bound did not stop the wait", elapsed, holders.holds)
	}
	if !errors.Is(res.err, exec.ErrWaitDelay) {
		t.Fatalf("the run error is %v, want exec's drain-cut sentinel", res.err)
	}
	if res.err.Error() != exec.ErrWaitDelay.Error() {
		t.Errorf("the runner wrapped exec's sentinel (%v): the reading, and the message that goes with it, belong to the caller's family", res.err)
	}
	// The bytes copied before the cut are kept — what the bound abandons is
	// the tail — which is why the exit-status family keeps the answer.
	if got := strings.TrimSpace(string(res.stdout)); got != answer {
		t.Errorf("stdout = %q, want the answer the command printed before the cut (%q)", got, answer)
	}
}

// The way out an author has, which the DSL page states: redirect the job's
// streams and nothing holds the node's output, so the node ends as soon as
// the command does and there is no drain to report. The control of the two
// witnesses above — a report on a node that left nothing holding would teach
// an operator to raise a bound that never ran.
//
// Mutation seen red: report the abandoned drain unconditionally, without
// asking proc.DrainCutShort — this node reads a drain nothing cut short.
func TestARedirectedBackgroundJobLeavesNothingToReport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	t.Setenv(proc.WaitDelayEnv, "")
	const answer = "the-nodes-answer"
	holders := newLeftoverHolders(t)
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: holders.redirectedRecipe(answer)}

	var logged lockedSink
	events := &toolNodeEvents{}
	ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
		WithToolPolicy(tool.OpenPolicy()),
		WithLogger(iterlog.New(iterlog.LevelInfo, &logged)),
		events.hooks())

	start := time.Now()
	out, err := ex.Execute(context.Background(), node, map[string]any{})
	elapsed := time.Since(start)
	t.Cleanup(func() { holders.join(1) })

	if err != nil {
		t.Fatalf("the node failed although nothing held its output: %v", err)
	}
	if out["result"] != answer {
		t.Fatalf("the node's output is %v, want the answer its command printed (%q)", out, answer)
	}
	if elapsed >= toolNodeDrainBound {
		t.Errorf("the node took %s, as long as the bound it had nothing to wait for (%s)", elapsed, toolNodeDrainBound)
	}
	report := events.callOutput() + "\n" + logged.String()
	if strings.Contains(report, proc.WaitDelayEnv) {
		t.Errorf("a drain nothing cut short is reported anyway: %s", report)
	}
}

// leftoverHolders builds the recipes of this file's witnesses — a command
// that prints its answer, leaves a process holding its output past the drain
// bound and exits 0 — and joins the holders they left.
type leftoverHolders struct {
	t   *testing.T
	dir string
	// holds is DERIVED from the site's own bound, so shortening the default
	// cannot make a witness vacuous: the holder outlives the bound that must
	// cut it. The margin is wall-clock slack on a loaded machine, where the
	// bound's timer can fire late — a holder that lets go as the bound fires
	// would make a verdict a race between a timer and a sleep.
	holds time.Duration
}

func newLeftoverHolders(t *testing.T) *leftoverHolders {
	t.Helper()
	return &leftoverHolders{t: t, dir: t.TempDir(), holds: toolNodeDrainBound + 2*time.Second}
}

// recipe is a shell body that prints printed, leaves ONE process holding the
// output past the bound, and exits 0.
//
// The holder traps PIPE with an empty handler: past the bound the pipes are
// closed under it, and the default SIGPIPE would kill it before it could
// report that it had finished — which [leftoverHolders.join] waits for, so no
// shell outlives the suite. Each holder reports under its own pid, so a run
// that ran the command twice is joined twice.
func (h *leftoverHolders) recipe(printed string) string {
	return fmt.Sprintf("( trap '' PIPE; sleep %.1f; : > %q/$$ ) &\nprintf '%%s\\n' %q\nexit 0",
		h.holds.Seconds(), h.dir, printed)
}

// redirectedRecipe is the same holder with its own streams: the way out the
// DSL page gives an author. The job holds a file, never the node's output, so
// nothing is left for the bound to cut.
func (h *leftoverHolders) redirectedRecipe(printed string) string {
	return fmt.Sprintf("( trap '' PIPE; sleep %.1f; : > %q/$$ ) >/dev/null 2>&1 &\nprintf '%%s\\n' %q\nexit 0",
		h.holds.Seconds(), h.dir, printed)
}

// join waits for the n holders the test's runs left behind.
func (h *leftoverHolders) join(n int) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if left, err := os.ReadDir(h.dir); err == nil && len(left) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	left, _ := os.ReadDir(h.dir)
	h.t.Errorf("%d of the %d leftover holders never reported their exit (%s) — one may outlive the suite",
		n-len(left), n, filepath.Clean(h.dir))
}

// lockedSink is a log sink a test reads while the node that writes it runs in
// another goroutine: the logger serialises its own writes, the reader is a
// second goroutine, and -race judges that.
type lockedSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *lockedSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *lockedSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// toolNodeEvents records what a tool node's own events carry — the stream an
// operator audits the node from — under a mutex, because the node runs in its
// own goroutine.
type toolNodeEvents struct {
	mu         sync.Mutex
	calls      []LLMToolCallInfo
	deliveries []string
}

func (e *toolNodeEvents) hooks() ClawExecutorOption {
	return WithEventHooks(EventHooks{
		OnToolCall: func(_ string, info LLMToolCallInfo) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.calls = append(e.calls, info)
		},
		OnToolNodeDeliverables: func(_, output string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.deliveries = append(e.deliveries, output)
			return nil
		},
	})
}

// callOutput is every tool_called event's output, joined: one per run of the
// node's command (a Verified Action runs more than one).
func (e *toolNodeEvents) callOutput() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.calls {
		out = append(out, c.ToolName+": "+c.Output)
	}
	return strings.Join(out, "\n")
}

// callError is the first error a tool_called event carried, nil when every
// run ended clean.
func (e *toolNodeEvents) callError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.calls {
		if c.Error != nil {
			return c.Error
		}
	}
	return nil
}

// countCalls is how many tool_called events a given virtual tool fired —
// what a witness joins its holders by.
func (e *toolNodeEvents) countCalls(toolName string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if c.ToolName == toolName {
			n++
		}
	}
	return n
}

func (e *toolNodeEvents) delivered() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.deliveries) > 0
}
