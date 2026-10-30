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
	"github.com/SocialGouv/iterion/pkg/sandbox"
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

// The override is THIS site's knob, and the page may only say so: it decides
// the bound this site chooses and stops at one a layer below already chose.
// The sibling above pins the "keeps a bound chosen below it" half with the
// variable CLEARED, which leaves the half an operator acts on — "raise
// ITERION_PROC_WAIT_DELAY" — unwitnessed; a page that reads "the override is
// over either" sends them to widen a driver client's drain with a variable
// the shared runner never applies to it. It holds for the one driver that
// bounds its client (kubernetes) only because that driver resolves its own
// default through this same variable (kubectlDrainBound via
// [proc.SetWaitDelay]) — a per-driver courtesy, not a rule of this site.
//
// Both arms in one table so neither reading can be stated without the other.
//
// Mutation seen red: drop the `if drainBound == 0` test in
// runWithSeparateStreams and call [proc.SetWaitDelay] unconditionally — the
// chosen-below arm then comes back with the override's hour, the bound the
// layer below asked for gone.
func TestTheOverrideReachesThisSitesBoundAndStopsAtOneChosenBelow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	// Recognisable, and neither this site's default nor the override.
	const chosenBelow = 750 * time.Millisecond
	// Far from any bound a layer could choose, so the arms cannot be confused.
	const override = time.Hour
	const answer = "the-commands-answer"

	for _, tc := range []struct {
		name string
		// below is what a layer under this site put on the Cmd; 0 is a Cmd
		// that carries no bound, which is what every driver but kubernetes
		// hands back.
		below     time.Duration
		wantBound time.Duration
	}{
		{"nothing chosen below: the override IS the bound in force", 0, override},
		{"a bound chosen below: the override does not reach it", chosenBelow, chosenBelow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(proc.WaitDelayEnv, override.String())
			cmd := exec.Command("bash", "-c", "printf %s "+answer)
			cmd.WaitDelay = tc.below

			stdout, _, bound, err := runWithSeparateStreams(cmd)
			if err != nil {
				t.Fatalf("the command exited 0 and left nothing holding its output: %v", err)
			}
			if bound != tc.wantBound {
				t.Errorf("the bound in force is %s, want %s — the report names this one, and it is the one an operator is told to raise", bound, tc.wantBound)
			}
			if got := string(stdout); got != answer {
				t.Errorf("stdout = %q, want %q", got, answer)
			}
		})
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
	// letsGo is the same derivation the other way, for the shape where the
	// bound must NOT bite: a job that writes and closes the output well
	// inside it. The witness that uses it widens the bound by its override
	// besides, so "inside the bound" is not that same race either.
	letsGo time.Duration
}

func newLeftoverHolders(t *testing.T) *leftoverHolders {
	t.Helper()
	return &leftoverHolders{
		t:      t,
		dir:    t.TempDir(),
		holds:  toolNodeDrainBound + 2*time.Second,
		letsGo: toolNodeDrainBound / 8,
	}
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

// insideBoundRecipe is the shape where the bound never bites: the command
// prints its own answer, then a job it left behind writes to that same
// output and CLOSES it well inside the bound. The node's wait ends with the
// job, nothing is left to cut — and the job's bytes are spliced into the
// answer the command meant to publish.
//
// No PIPE trap here, unlike the holders above, and that is deliberate: the
// pipe is never closed under this job, so it reports its exit the ordinary
// way. A run where the bound DID bite would kill it on its write instead,
// and [leftoverHolders.join] says so — the inconclusive-run guard of the
// witness that uses this recipe.
func (h *leftoverHolders) insideBoundRecipe(printed, late string) string {
	return fmt.Sprintf("( sleep %.2f; printf '%%s' %q; : > %q/$$ ) &\nprintf '%%s' %q\nexit 0",
		h.letsGo.Seconds(), late, h.dir, printed)
}

// cutAnswerRecipe is the one shape where the bytes the bound leaves in hand
// really are a PREFIX: the answer is written by the process the command left,
// which prints its head at once and its tail past the bound. The command
// itself prints nothing and exits 0.
//
// The trap is what lets the holder report: its tail write lands on a pipe the
// bound has closed, and the default SIGPIPE would kill it before it could say
// it had finished — which [leftoverHolders.join] waits for.
func (h *leftoverHolders) cutAnswerRecipe(head, tail string) string {
	return fmt.Sprintf("( trap '' PIPE; printf '%%s' %q; sleep %.1f; printf '%%s\n' %q; : > %q/$$ ) &\nexit 0",
		head, h.holds.Seconds(), tail, h.dir)
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

// callOutputFor is every tool_called event of ONE virtual tool, joined: a
// Verified Action fires one per rung, and an assertion about the recipe's own
// run must not read the postcondition's.
func (e *toolNodeEvents) callOutputFor(toolName string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.calls {
		if c.ToolName == toolName {
			out = append(out, c.Output)
		}
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

// What a job must do to outlive the node, MEASURED on the host — the
// prescription the DSL page gives, and the reason it is a prescription and
// not an afterthought. The bound does not merely stop waiting: it closes the
// descriptors exec owns (the parent ends of the pipes it made), and a job
// still writing to the node's output takes the default SIGPIPE at its next
// write. So `server &` as the page once showed it is killed by the engine
// moments after the node reports success, and `server >log 2>&1 &` is what
// survives.
//
// The same arm measures the second cost of the unredirected form: the job's
// lines interleave with the command's own answer in one pipe, so the node's
// structured output is destroyed — it comes back as the `result` wrapper,
// marked as a possible prefix, instead of the object the recipe printed.
//
// Mutation seen red: drop the [proc.SetWaitDelay] call in
// runWithSeparateStreams — with no bound nothing closes the pipe, the job
// writes its way to the end and the unredirected arm reads a survivor.
func TestABackgroundJobThatKeepsWritingDiesAtTheBoundAndRedirectionIsHowItOutlivesTheNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the job is a background one")
	}
	// The bound measured below is this site's own, so the environment the
	// suite runs in does not get to choose it.
	t.Setenv(proc.WaitDelayEnv, "")

	for _, tc := range []struct {
		name string
		// streams is what the job's own stdout and stderr are redirected
		// to, before the `&`: "" is the `server &` form, the redirect is
		// the way out.
		streams string
		// traps is whether the job handles PIPE and says so: the arm that
		// names the signal, rather than inferring one from a job that
		// stopped.
		traps bool
		// wantSurvives is whether the job lives past the node it was left
		// behind by.
		wantSurvives bool
	}{
		{name: "a job writing to the output it inherited is killed at its next write", streams: "", wantSurvives: false},
		{name: "the signal that killed it is SIGPIPE, and the job itself says so", streams: "", traps: true, wantSurvives: false},
		{name: "a job that redirected its own streams outlives the node", streams: ">/dev/null 2>&1 ", wantSurvives: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := newWritingJob(t)
			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: job.recipe(`{"started":true}`, tc.streams, tc.traps)}
			ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()))

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
				t.Fatalf("the tool node did not come back in 30s: it waits for the output the job it left holds")
			}
			elapsed := time.Since(start)

			if res.err != nil {
				t.Fatalf("the node failed although its command exited 0: %v", res.err)
			}
			if !job.wrote(1, 10*time.Second) {
				t.Fatalf("the job never wrote once: this run is not the shape the witness needs — a job that holds the node's output and keeps writing to it")
			}
			// The verdict below is "the job did not write again", which
			// only means anything if the node's wait ended BEFORE that
			// write was due. A bound that fired late on a loaded machine
			// makes the run inconclusive, never a false fact.
			if elapsed >= job.pause {
				t.Fatalf("the node came back in %s, no sooner than the job's next write (%s): the bound did not fire before it, so this run cannot say whether the write survived", elapsed, job.pause)
			}
			survived := job.wrote(2, job.pause+5*time.Second)
			if survived != tc.wantSurvives {
				t.Errorf("the job wrote again past the bound: %v, want %v — on the host the bound closes the pipes exec owns, so a job still writing to the node's output takes SIGPIPE and only a job that redirected its streams outlives the node", survived, tc.wantSurvives)
			}
			// The signal, from the job's own hand: a handler that fired is
			// the difference between "the job stopped" and "the write took
			// SIGPIPE", which is what the DSL page now tells an author.
			if signalled := job.tookSIGPIPE(); signalled != tc.traps {
				t.Errorf("the job reported SIGPIPE: %v, want %v", signalled, tc.traps)
			}
			if !tc.wantSurvives {
				// The answer is destroyed too: the job's lines and the
				// command's object share one pipe.
				if res.out["started"] != nil {
					t.Errorf("the node's output parsed as the object its command printed (%v): a job writing to the same pipe interleaves with it, which is the other half of why the redirect is the prescription", res.out)
				}
				if res.out[toolNodeDrainCutKey] != true {
					t.Errorf("the output that no longer parses is published unmarked (%v): a consumer cannot tell it from the answer", res.out)
				}
				return
			}
			// Redirected, nothing shares the node's pipe: the answer is
			// the object the recipe printed, whole and unmarked.
			if res.out["started"] != true {
				t.Errorf("the node's output is %v, want the object its command printed", res.out)
			}
			if _, marked := res.out[toolNodeDrainCutKey]; marked {
				t.Errorf("a node whose output nothing held is marked as a possible prefix: %v", res.out)
			}
			if !job.wrote(3, job.pause+10*time.Second) {
				t.Error("the redirected job never finished its writes: it was stopped by something this witness does not know about")
			}
		})
	}
}

// A cut the exit-status family keeps the success of can still have cost
// BYTES, and the one shape where that is invisible is the `result` wrapper:
// a JSON object that parsed is the whole of what the command meant to say,
// the wrapper may be the head of anything. So a wrapper built from a run the
// bound cut carries the mark, and a whole object does not — the distinction a
// consumer of {{outputs.<node>.*}} has no other way to make.
//
// Mutations seen red: drop the mark in runToolNodeCore (a prefix is published
// as the answer); mark every cut run, asking only proc.DrainCutShort (the
// whole-object arm, the measured and common case of a recipe that left a
// server running, is published as a possible prefix).
func TestACutOutputThatCouldBeAPrefixIsPublishedAsOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	t.Setenv(proc.WaitDelayEnv, "")

	t.Run("a whole JSON object the command printed itself is published unmarked", func(t *testing.T) {
		holders := newLeftoverHolders(t)
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: holders.recipe(`{"state":"whole"}`)}
		events := &toolNodeEvents{}
		ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()), events.hooks())

		res, err := runToolNodeUnderDeadline(t, ex, node)
		t.Cleanup(func() { holders.join(1) })
		if err != nil {
			t.Fatalf("the node failed although its command exited 0 and printed its whole answer: %v", err)
		}
		// The cut HAPPENED — this arm is not a run with nothing to mark.
		if !strings.Contains(events.callOutput(), "still holds its output") {
			t.Fatalf("the bound did not cut this run's drain: this arm cannot say what an unmarked whole object proves (%s)", events.callOutput())
		}
		if res["state"] != "whole" {
			t.Fatalf("the node's output is %v, want the object its command printed", res)
		}
		if _, marked := res[toolNodeDrainCutKey]; marked {
			t.Errorf("a whole JSON object is published as a possible prefix (%v): the mark would then say nothing, since it is on every cut run", res)
		}
	})

	t.Run("an answer the bound cut mid-object is published as a possible prefix", func(t *testing.T) {
		holders := newLeftoverHolders(t)
		const head = `{"deployed":12,"failed":`
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "deploy"}, Command: holders.cutAnswerRecipe(head, `0}`)}
		events := &toolNodeEvents{}
		ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()), events.hooks())

		res, err := runToolNodeUnderDeadline(t, ex, node)
		t.Cleanup(func() { holders.join(1) })
		if err != nil {
			t.Fatalf("the node failed although its command exited 0: %v", err)
		}
		if err := events.callError(); err != nil {
			t.Errorf("the node's event carries an error on a command that exited 0: %v", err)
		}
		if res["result"] != head {
			t.Fatalf("the node's output is %v, want the prefix the bound left in hand (%q)", res, head)
		}
		if res[toolNodeDrainCutKey] != true {
			t.Fatalf("half an answer is published as the answer (%v): a downstream {{outputs.deploy.result}} reads a prefix with nothing saying so", res)
		}
		// The readers of a node's output take the mark: the schema
		// validator reads its declared fields and ignores the rest, which
		// is what lets a private key travel without joining the contract.
		schema := &ir.Schema{Name: "answer", Fields: []*ir.SchemaField{{Name: "result", Type: ir.FieldTypeString}}}
		if err := ValidateOutput(res, schema); err != nil {
			t.Errorf("the marked output does not validate against the node's declared schema: %v", err)
		}
	})
}

// The THIRD outcome of a leftover job, which neither reading above covers:
// the job writes its bytes and LETS GO inside the bound, so nothing is ever
// cut. The node ends with that job, keeps its success — and publishes an
// answer the job CONTAMINATED, with no mark and nothing reported, because
// there was no cut to mark or report.
//
// It is the common `server &` shape, a startup line landing half a second
// after the command's own answer, and it is the case where a downstream
// {{outputs.<node>.<field>}} stops resolving with nothing to say why — its
// raw braces reach the next command as text, and no mark and no report
// explain them: the object the recipe declared was reshaped into the
// `result` wrapper by bytes it never meant to publish. So the mark's guarantee is ONE-WAY — a mark says these bytes may
// be a prefix; its absence says nothing was cut, never that the answer is
// what the command printed. That is what the DSL page, the
// ITERION_PROC_WAIT_DELAY row and [toolNodeDrainBound] say, and the reason
// the way out is the recipe's own: redirect what must outlive the node, wait
// for the job whose bytes you publish.
//
// The report's absence IS the cut's absence: runToolNodeCore emits that
// report on exactly [proc.DrainCutShort], so a run with no report had no
// sentinel. The bound in force is the operator's override, two orders of
// magnitude above the job's own write, so this arm is not a race between a
// timer and a sleep.
//
// Mutations seen red: mark every wrapped output, asking the parse alone and
// not the cut (`drainCut && wrapped` → `wrapped` in runToolNodeCore) — a
// contaminated answer nothing cut is then published as a possible prefix,
// read here on a plain node whose own recipe contaminated it, and on the
// postcondition's uncut run by the merge witness below; and read every run
// as a cut one (`drainCut := true`), which reports to an operator a drain
// the bound never abandoned.
func TestAJobThatLetsGoInsideTheBoundContaminatesTheAnswerWithNothingToReport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the job is a background one")
	}
	const wide = time.Minute
	t.Setenv(proc.WaitDelayEnv, wide.String())
	const (
		printed = `{"deployed":12,"failed":0}`
		late    = "LATE"
	)

	holders := newLeftoverHolders(t)
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "deploy"}, Command: holders.insideBoundRecipe(printed, late)}
	var logged lockedSink
	events := &toolNodeEvents{}
	ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
		WithToolPolicy(tool.OpenPolicy()),
		WithLogger(iterlog.New(iterlog.LevelInfo, &logged)),
		events.hooks())

	start := time.Now()
	res, err := runToolNodeUnderDeadline(t, ex, node)
	elapsed := time.Since(start)
	t.Cleanup(func() { holders.join(1) })

	if err != nil {
		t.Fatalf("the node failed although its command exited 0: %v", err)
	}
	// The job lets go in a fraction of a second under a bound of a minute:
	// a node back in seconds came back with the job, never with the bound.
	if elapsed >= 10*time.Second {
		t.Fatalf("the node took %s under a %s bound: the job did not let go inside it, so this arm cannot say what an uncut run publishes", elapsed, wide)
	}
	if res["result"] != printed+late {
		t.Fatalf("the node's output is %v, want the command's object with the job's bytes spliced in (%q)", res, printed+late)
	}
	if _, declared := res["deployed"]; declared {
		t.Errorf("the node's output still carries the field its command printed (%v): the contaminated bytes are supposed to have stopped parsing as that object", res)
	}
	if _, marked := res[toolNodeDrainCutKey]; marked {
		t.Errorf("an answer nothing cut short is published as a possible prefix (%v): the mark then says nothing about a cut", res)
	}
	report := events.callOutput() + "\n" + logged.String()
	if strings.Contains(report, proc.WaitDelayEnv) {
		t.Errorf("a drain nothing cut short is reported anyway: %s", report)
	}
}

// The settle of a sandboxed run judges the command by its completion line,
// and a nil run error no longer means its exec client reported success:
// runToolNodeCore clears the drain bound's sentinel BEFORE the settle, so a
// client that reported a cut drain arrives here indistinguishable from one
// that reported nothing at all. The message cannot claim otherwise — an
// operator reading "the client reported success" about a client that reported
// a cut would go looking for a client bug that is not there.
//
// Mutation seen red: restore "but its exec client reported success".
func TestASandboxedRunWhoseLineSaysNonZeroDoesNotCallItsClientASuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the completion wrapper is a POSIX sh program")
	}
	ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()))

	c := sandbox.NewCompletion("tool")
	// The line is the command's own: it is produced by running the
	// completion protocol, never hand-assembled, so what this witness
	// settles is the shape a sandboxed run really hands over.
	argv := c.Wrap([]string{"sh", "-c", "printf 'the-answer\\n'; exit 7"})
	stdout, outErr := exec.Command(argv[0], argv[1:]...).Output()
	if len(stdout) == 0 {
		t.Fatalf("the completion wrapper wrote nothing (%v): there is no line to settle", outErr)
	}

	clean, err := ex.settleSandboxedRun(context.Background(), "gate", c, stdout, nil)
	if err == nil {
		t.Fatalf("a command whose line says 7 settled as a success: %q", clean)
	}
	if !strings.Contains(err.Error(), "exited 7") {
		t.Errorf("the settle does not name the status the line carried: %v", err)
	}
	if strings.Contains(err.Error(), "reported success") {
		t.Errorf("the settle calls a client that reported no status at all a success: %v", err)
	}
	if got := strings.TrimSpace(string(clean)); got != "the-answer" {
		t.Errorf("clean = %q, want the command's own output without the line", got)
	}
}

// runToolNodeUnderDeadline runs a node and gives up on it, so a regression
// that parks the node fails in seconds instead of holding the package to its
// timeout.
func runToolNodeUnderDeadline(t *testing.T, ex *ClawExecutor, node *ir.ToolNode) (map[string]any, error) {
	t.Helper()
	type outcome struct {
		out map[string]any
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		out, err := ex.Execute(context.Background(), node, map[string]any{})
		got <- outcome{out: out, err: err}
	}()
	select {
	case res := <-got:
		return res.out, res.err
	case <-time.After(30 * time.Second):
		t.Fatalf("the tool node did not come back in 30s: it waits for the output a process its command left holds")
		return nil, nil
	}
}

// writingJob is the recipe of a backgrounded job that keeps WRITING to the
// stream it was handed — what a server does, and what the DSL page's
// `server &` is — reporting each write it completed as a file, so a witness
// can read how far it got before anything stopped it.
type writingJob struct {
	t   *testing.T
	dir string
	// pause is DERIVED from this site's bound, so the job's second write
	// lands well past the bound however the bound changes: the margin is
	// the wall-clock slack leftoverHolders takes, for the same reason —
	// the bound's timer can fire late on a loaded machine.
	pause time.Duration
}

func newWritingJob(t *testing.T) *writingJob {
	t.Helper()
	return &writingJob{t: t, dir: t.TempDir(), pause: toolNodeDrainBound + 2*time.Second}
}

// recipe prints printed as the node's answer and leaves a job that writes
// three times: once at once, then twice past the bound. streams is the
// redirection the job's own streams get — "" for `server &`, and
// ">/dev/null 2>&1 " for the form that keeps the node's output to itself.
// Each write is a line first and the report of that line second, so a job
// killed by the write reports nothing.
//
// traps makes the job NAME the signal instead of leaving a witness to infer
// it: the handler records that PIPE was delivered and ends the job there, so
// the recorded signal and the write it never completed are the same event.
func (j *writingJob) recipe(printed, streams string, traps bool) string {
	trap := ""
	if traps {
		trap = fmt.Sprintf("trap 'printf SIGPIPE > %q; exit 141' PIPE; ", j.signalFile())
	}
	return fmt.Sprintf("( %sprintf 'tick 1\\n'; : > %q/1; sleep %.1f; printf 'tick 2\\n'; : > %q/2; printf 'tick 3\\n'; : > %q/3 ) %s&\nprintf '%%s\\n' %q\nexit 0",
		trap, j.dir, j.pause.Seconds(), j.dir, j.dir, streams, printed)
}

// signalFile is where a trapping job records the signal it took. It lives
// beside the job's directory, never in it, so a recorded signal is never
// counted as one of the writes.
func (j *writingJob) signalFile() string {
	return j.dir + ".sigpipe"
}

// tookSIGPIPE reports whether the job's own PIPE handler fired.
func (j *writingJob) tookSIGPIPE() bool {
	j.t.Helper()
	_, err := os.Stat(j.signalFile())
	return err == nil
}

// wrote waits up to within for the job's nth write to be reported.
func (j *writingJob) wrote(n int, within time.Duration) bool {
	j.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(filepath.Join(j.dir, fmt.Sprint(n))); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The mark describes the `result` wrapper it travels with, and a Verified
// Action publishes ONE output built from TWO runs: [ClawExecutor.verifiedOutput]
// merges the recipe's map with the postcondition's, postcondition wins on key
// clash. So the mark has to follow the `result` that SURVIVED that clash —
// carried along blindly it lands on the postcondition's own answer, whole and
// uncut, and tells a consumer of `{{outputs.<node>.result}}`, of the artifact
// a `publish:` writes or of an edge gate on the mark that those bytes may be a
// prefix when they are the whole.
//
// Three of the four readers of runToolNodeCore are on this path
// (runVerifiedRecipe, runPostcondition, selfRepair, the inventory in
// tool_node_core_callers_test.go), and the merge is the one point all five of
// its call sites cross: the arms below drive it through
// executeVerifiedToolNode's rung 2 — the recipe runs, its drain is cut, and
// the postcondition judges — because that is where the two maps meet.
//
// Mutation seen red: carry the mark from whichever map holds it (the merge's
// own default, `out[k] = v` with nothing after it) — the whole-postcondition
// arm reads `{"_drain_cut": true, "result": "ok"}`, a complete 2-byte answer
// published as a possible prefix.
func TestTheDrainCutMarkFollowsTheResultThatSurvivedTheVerifiedMerge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture, and the holder is a background job")
	}
	// The bound the arms rely on is this site's, so the environment the suite
	// runs in does not get to choose it.
	t.Setenv(proc.WaitDelayEnv, "")

	const head = `{"deployed":12,"failed":`

	for _, tc := range []struct {
		name string
		// postcondAnswer is what the postcondition prints once the recipe's
		// marker is there: the second map of the merge.
		postcondAnswer string
		// postcondHolds is whether the postcondition itself leaves a process
		// holding ITS output, so its own answer is the cut one.
		postcondHolds bool
		// wantResult is the `result` the merged output must carry.
		wantResult string
		// wantMarked is whether the mark belongs on that result.
		wantMarked bool
		// holders is how many leftover holders the node's runs leave.
		holders int
	}{
		{
			name:           "the postcondition's whole answer wins the clash, so the recipe's mark does not travel with it",
			postcondAnswer: "ok",
			wantResult:     "ok",
			wantMarked:     false,
			holders:        1,
		},
		{
			name:           "the postcondition declares other fields, so the result the mark describes is still the recipe's",
			postcondAnswer: `{"state":"whole"}`,
			wantResult:     head,
			wantMarked:     true,
			holders:        1,
		},
		{
			name:          "the postcondition's own drain was cut, so the result that won the clash carries its own mark",
			postcondHolds: true,
			wantResult:    head,
			wantMarked:    true,
			holders:       2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holders := newLeftoverHolders(t)
			marker := filepath.Join(t.TempDir(), "the-recipe-ran")
			// Rung 1 must NOT skip: the postcondition is unmet until the
			// recipe has run, which is what puts the two maps in the merge.
			postcond := "test -f " + marker + " || exit 1\n"
			if tc.postcondHolds {
				postcond += holders.cutAnswerRecipe(head, `0}`)
			} else {
				postcond += fmt.Sprintf("printf '%%s' %q\nexit 0", tc.postcondAnswer)
			}
			node := &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "deploy"},
				// The recipe prints nothing itself: its whole answer is
				// written by the process it leaves, head now and tail past
				// the bound, so what the bound leaves in hand is a PREFIX.
				Command:       ": > " + marker + "\n" + holders.cutAnswerRecipe(head, `0}`),
				Postcondition: postcond,
				Goal:          "the deploy is reported",
				Policy:        ir.PolicyRequired,
			}
			events := &toolNodeEvents{}
			ex := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
				WithToolPolicy(tool.OpenPolicy()), events.hooks())

			res, err := runToolNodeUnderDeadline(t, ex, node)
			t.Cleanup(func() { holders.join(tc.holders) })
			if err != nil {
				t.Fatalf("the verified node failed although its recipe exited 0 and its postcondition was met: %v", err)
			}
			// The inconclusive-run guard: the arms below say what the mark
			// does with a CUT recipe, so a run whose recipe was not cut —
			// or whose bytes parsed, which is the unmarked shape — proves
			// nothing about the merge.
			recipe := events.callOutputFor(shellToolNodeToolName(node))
			if !strings.Contains(recipe, "still holds its output") {
				t.Fatalf("the recipe's drain was not cut, so its map carried no mark and this arm cannot judge the merge: %s", recipe)
			}
			if !strings.Contains(recipe, head) {
				t.Fatalf("the recipe's bytes are %q, want the non-parsing head %q the mark rides: a map that parsed is published unmarked", recipe, head)
			}

			if res["result"] != tc.wantResult {
				t.Errorf("the merged output's result is %#v, want %q", res["result"], tc.wantResult)
			}
			if marked := res[toolNodeDrainCutKey] == true; marked != tc.wantMarked {
				t.Errorf("the merged output is marked as a possible prefix: %v, want %v — the mark belongs to the run whose `result` survived the clash (%v)", marked, tc.wantMarked, res)
			}
			// The rung is the ladder's, not an artefact of the merge: the
			// recipe ran and the postcondition judged it met.
			meta, ok := res["_verified_action"].(map[string]any)
			if !ok {
				t.Fatalf("the merged output carries no _verified_action metadata: %v", res)
			}
			if meta["rung"] != "recipe" || meta["postcondition_met"] != true {
				t.Errorf("the ladder reports rung %v, met %v, want rung \"recipe\" met true: the arms above describe the rung-2 merge", meta["rung"], meta["postcondition_met"])
			}
		})
	}
}

// The hole in [toolNodeDrainCutKey] is named by the PARSE, and this is that
// parse: `wrapped` is the only thing the mark is conditioned on, so whatever
// a cut command's bytes happen to parse TO is published unmarked.
//
// The `null` arm is the shape the key's doc now names beside the
// object-per-line one: `cmd | jq '.absent'` prints it, it parses to NO map,
// and the output a `publish:` writes is then nil — it carries neither the
// mark nor the engine's own `_duration_ms`, measured on a run whose drain was
// cut and reported.
//
// Mutation seen red: report wrapped for a parse that yielded a nil map
// (`output == nil` joined to the unmarshal error) — the `null` arm then reads
// the wrapper, and the doc's own hole closes on paper while the published
// output stays nil.
// It takes no t.Parallel(): this file is of the package's SERIAL family (it
// calls t.Setenv), and a parallel test in a serial file panics the whole
// binary (TestASerialTestFileNeverGoesParallel).
func TestWhatAToolNodesStdoutParsesToDecidesWhetherACutCanBeMarked(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stdout is what the command printed, as the node's output parse
		// receives it.
		stdout string
		// wantWrapped is the answer the mark is conditioned on.
		wantWrapped bool
		// wantNil is whether the published output map is nil — no key of
		// the engine's own can be attached to it either.
		wantNil bool
	}{
		{name: "an object parses, so a cut leaves no evidence", stdout: `{"deployed":12}`},
		{name: "an empty object parses too: the named hole", stdout: `{}`},
		{name: "a bare null parses to no map at all", stdout: `null`, wantNil: true},
		{name: "a prefix of an object cannot parse, which is what the mark rides", stdout: `{"deployed":12,"failed":`, wantWrapped: true},
		{name: "a lone space does not parse", stdout: ` `, wantWrapped: true},
		{name: "a number is not an object", stdout: `12`, wantWrapped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, wrapped := parseToolNodeOutputWrapped(tc.stdout, strings.TrimSpace(tc.stdout))
			if wrapped != tc.wantWrapped {
				t.Errorf("parse of %q reports wrapped %v, want %v: the mark is conditioned on this answer alone", tc.stdout, wrapped, tc.wantWrapped)
			}
			if (out == nil) != tc.wantNil {
				t.Errorf("parse of %q yields %#v, nil %v, want nil %v", tc.stdout, out, out == nil, tc.wantNil)
			}
			if tc.wantWrapped && out["result"] != strings.TrimSpace(tc.stdout) {
				t.Errorf("the wrapper of %q holds %#v, want the bytes themselves", tc.stdout, out["result"])
			}
			if tc.wantNil {
				// A nil map takes no key at all: a mark written on this
				// parse's output would not be lost, it would PANIC. That is
				// the second reason the write is conditioned on `wrapped`
				// and not on the cut alone.
				func() {
					defer func() {
						if recover() == nil {
							t.Error("writing the mark on the output of a `null` parse did not panic: this arm's point is that the parse leaves no map to attach a key to")
						}
					}()
					out[toolNodeDrainCutKey] = true
				}()
			}
		})
	}
}
