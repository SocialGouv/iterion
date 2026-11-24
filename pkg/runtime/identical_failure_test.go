package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The scenarios of this file are written for a limit of scenarioLimit
// identical failures, reached within one engine life: the first execution and
// the one in-process retry (retryOnce). They pin that limit, so the shipped
// default can move without them; what the DEFAULT does is witnessed where it
// is named (the recovery, runner and cli tests derive from the constant).
const scenarioLimit = 2

func pinScenarioLimit(t *testing.T) {
	t.Helper()
	t.Setenv(EnvNodeMaxIdenticalFailures, strconv.Itoa(scenarioLimit))
}

// exitError is the error a real process leaves when it exits with status.
func exitError(t *testing.T, status int) error {
	t.Helper()
	runErr := exec.Command("sh", "-c", fmt.Sprintf("exit %d", status)).Run()
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("scenario broken: `exit %d` returned %v, not an exit error", status, runErr)
	}
	return runErr
}

// shellFailure is a tool node's failure as the executor builds it: the node,
// the verb, the process's exit error, then the stream quoted — through the
// typed failure the executor returns, so the corpus runs the real reader
// (ToolCommandFailure), not a hand-built text.
func shellFailure(t *testing.T, status int, stderr string) *model.ToolExecutionError {
	t.Helper()
	return shellFailureOn(t, "gate", status, "", stderr)
}

// shellFailureOn is shellFailure with the node id and stdout set: the paths
// where the streams decide the reading.
func shellFailureOn(t *testing.T, nodeID string, status int, stdout, stderr string) *model.ToolExecutionError {
	t.Helper()
	return &model.ToolExecutionError{Cause: &model.ToolCommandFailure{
		NodeID:  nodeID,
		Outcome: "shell command failed",
		Cause:   exitError(t, status),
		Stdout:  stdout,
		Stderr:  stderr,
	}}
}

// fingerprintOf is the fingerprint the engine takes of failure for a run and a
// node: the exit status its chain proves, and its text.
func fingerprintOf(t *testing.T, runID, nodeID string, failure *model.ToolExecutionError) string {
	t.Helper()
	status, ok := failure.ExitStatus()
	if !ok {
		t.Fatalf("scenario broken: the failure carries no exit status: %v", failure)
	}
	return toolFailureFingerprint(runID, nodeID, status, failure)
}

// sameWords is an error whose text is fixed whatever it wraps.
type sameWords struct{ err error }

func (s *sameWords) Error() string { return "the command failed" }
func (s *sameWords) Unwrap() error { return s.err }

// The fingerprint is what decides "the same failure": equal for an unchanged
// failure, different as soon as the status or the output moves, salted by the
// run and the node — and a digest, so the checkpoint it is written to never
// holds the command's output.
func TestTheToolFailureFingerprintFollowsStatusAndOutput(t *testing.T) {
	const secretLooking = "Authorization: Bearer hunter2-9f8e7d6c5b4a"
	base := fingerprintOf(t, "run-1", "gate", shellFailure(t, 3, secretLooking))

	if again := fingerprintOf(t, "run-1", "gate", shellFailure(t, 3, secretLooking)); again != base {
		t.Errorf("two executions of one failure have different fingerprints:\n%s\n%s", base, again)
	}
	for name, other := range map[string]string{
		"another exit status": fingerprintOf(t, "run-1", "gate", shellFailure(t, 4, secretLooking)),
		"another output":      fingerprintOf(t, "run-1", "gate", shellFailure(t, 3, secretLooking+" ")),
		"another run":         fingerprintOf(t, "run-2", "gate", shellFailure(t, 3, secretLooking)),
		"another node":        fingerprintOf(t, "run-1", "gate2", shellFailure(t, 3, secretLooking)),
	} {
		if other == base {
			t.Errorf("%s has the same fingerprint", name)
		}
	}
	// The status is a part of its own, not a reading of the text: two
	// failures that say the same words on two statuses are two failures.
	three := &model.ToolExecutionError{Cause: &sameWords{exitError(t, 3)}}
	four := &model.ToolExecutionError{Cause: &sameWords{exitError(t, 4)}}
	if three.Error() != four.Error() {
		t.Fatalf("scenario broken: the two failures do not read alike: %q / %q", three, four)
	}
	if fingerprintOf(t, "run-1", "gate", three) == fingerprintOf(t, "run-1", "gate", four) {
		t.Error("the same words on two exit statuses share a fingerprint")
	}
	if !strings.HasPrefix(base, "sha256:") || len(base) != len("sha256:")+64 {
		t.Errorf("fingerprint %q is not a sha256 digest", base)
	}
	if strings.Contains(base, "hunter2") || strings.Contains(base, "Bearer") {
		t.Errorf("the fingerprint quotes the command's output: %q", base)
	}
}

// The parts are length-prefixed: a run id that ends where a node id begins
// cannot hash like its neighbour.
func TestTheToolFailureFingerprintKeepsItsPartsApart(t *testing.T) {
	f := shellFailure(t, 3, "x")
	if fingerprintOf(t, "run-1", "gate", f) == fingerprintOf(t, "run-", "1gate", f) {
		t.Error("two different (run, node) pairs share a fingerprint")
	}
}

// The executor names a `script:` body's temp file afresh for every execution;
// the fingerprint reads through that name and through nothing else.
func TestTheToolFailureFingerprintReadsThroughTheScriptTempName(t *testing.T) {
	script := func(name, detail string) *model.ToolExecutionError {
		return &model.ToolExecutionError{Cause: &model.ToolCommandFailure{
			NodeID:  "gate",
			Outcome: "script failed",
			Cause:   exitError(t, 127),
			Stderr:  "/tmp/" + name + ": 2: " + detail + ": not found",
		}}
	}
	first := fingerprintOf(t, "run-1", "gate", script(".iterion-script-1234567890.sh", "nope"))
	if second := fingerprintOf(t, "run-1", "gate", script(".iterion-script-42.sh", "nope")); second != first {
		t.Error("the same script failure under two temp names has two fingerprints")
	}
	if other := fingerprintOf(t, "run-1", "gate", script(".iterion-script-1234567890.sh", "nope2")); other == first {
		t.Error("two different script failures share a fingerprint")
	}
	// A number of the COMMAND's own is not the executor's name: it stays.
	if other := fingerprintOf(t, "run-1", "gate", script("request-1234567890.sh", "nope")); other == fingerprintOf(t, "run-1", "gate", script("request-42.sh", "nope")) {
		t.Error("a path of the command's own was normalised away")
	}
}

// Which nodes an identical repeat is read from: a `command:` the shell runs
// and a `script:` body, with no model rung in reach. Nothing else.
func TestOnlyAProcessRecipeWithNoModelRungRepeatsAsPermanent(t *testing.T) {
	base := ir.BaseNode{ID: "n"}
	recover := &ir.RecoverySpec{MaxRepairAttempts: 1}
	agentRung := &ir.RecoverySpec{MaxAgentAttempts: 1}
	for _, c := range []struct {
		name string
		node ir.Node
		want bool
	}{
		{"a shell command", &ir.ToolNode{BaseNode: base, Command: "make test"}, true},
		{"a script body", &ir.ToolNode{BaseNode: base, Script: "exit 3", Language: "sh"}, true},
		{"a verified action with no recovery", &ir.ToolNode{BaseNode: base, Command: "make test", Postcondition: "test -f ok", Policy: ir.PolicyRequired}, true},
		{"an inert recovery block (policy required)", &ir.ToolNode{BaseNode: base, Command: "make test", Postcondition: "test -f ok", Policy: ir.PolicyRequired, Recovery: recover}, true},
		{"a registry tool", &ir.ToolNode{BaseNode: base, Command: "read_file"}, false},
		{"a connector action", &ir.ToolNode{BaseNode: base, Action: "github.merge"}, false},
		{"a self-repair rung", &ir.ToolNode{BaseNode: base, Command: "make test", Postcondition: "test -f ok", Policy: ir.PolicyRecover, Recovery: recover}, false},
		{"an agent rung", &ir.ToolNode{BaseNode: base, Command: "make test", Postcondition: "test -f ok", Policy: ir.PolicyRecover, Recovery: agentRung}, false},
		{"an agent", &ir.AgentNode{BaseNode: base}, false},
		{"a judge", &ir.JudgeNode{BaseNode: base}, false},
		{"no node", nil, false},
	} {
		if got := repeatsAsPermanent(c.node); got != c.want {
			t.Errorf("%s: repeatsAsPermanent = %v, want %v", c.name, got, c.want)
		}
	}
}

// The limit's variable: unset is the default, a count is itself, 0 and the
// off words disable, and a value that is no count keeps the default.
func TestTheIdenticalFailureLimitIsReadFromItsVariable(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int
	}{
		{"", DefaultNodeMaxIdenticalFailures},
		{"0", 0},
		{"off", 0},
		{"None", 0},
		{"1", 1},
		{" 5 ", 5},
		{"2.0", 2},
		{"-1", DefaultNodeMaxIdenticalFailures},
		{"two", DefaultNodeMaxIdenticalFailures},
		{"2.5", DefaultNodeMaxIdenticalFailures},
		{"nan", DefaultNodeMaxIdenticalFailures},
		// Past what an int holds is a very large count — a limit nothing
		// reaches — never the default, the strictest value shipped.
		{"99999999999999999999", math.MaxInt},
		{"1e21", math.MaxInt},
		{"inf", math.MaxInt},
		{"-99999999999999999999", DefaultNodeMaxIdenticalFailures},
	} {
		t.Setenv(EnvNodeMaxIdenticalFailures, c.raw)
		if got := nodeMaxIdenticalFailures(); got != c.want {
			t.Errorf("%s=%q: limit = %d, want %d", EnvNodeMaxIdenticalFailures, c.raw, got, c.want)
		}
	}
}

// The default is ONE constant, and the two pages an operator reads state its
// value once each: the variable's row, and the table of what a limit means.
// Changing the default is that line and these two numbers; this is what
// names them.
func TestTheDocsStateTheDefaultLimitOnceEach(t *testing.T) {
	want := strconv.Itoa(DefaultNodeMaxIdenticalFailures)
	for _, doc := range []struct {
		path string
		// stated captures the value the page gives as the default.
		stated *regexp.Regexp
	}{
		{"../../docs/environment-variables.md", regexp.MustCompile("(?m)^\\| `" + EnvNodeMaxIdenticalFailures + "` \\|.*\\| `([0-9]+)` \\|$")},
		{"../../docs/resume.md", regexp.MustCompile("(?m)^\\| `([0-9]+)` \\(the default\\) \\|")},
	} {
		raw, err := os.ReadFile(doc.path)
		if err != nil {
			t.Fatal(err)
		}
		found := doc.stated.FindAllStringSubmatch(string(raw), -1)
		switch {
		case len(found) != 1:
			t.Errorf("%s states the default limit %d time(s) in the form %s, want exactly once", doc.path, len(found), doc.stated)
		case found[0][1] != want:
			t.Errorf("%s gives %s as the default limit, and DefaultNodeMaxIdenticalFailures is %s", doc.path, found[0][1], want)
		}
	}
}

// identicalFailer fails every execution of its node the way a command that
// exits 3 does: a real process's exit error, and the same text every time.
type identicalFailer struct {
	calls int
	exit  error
}

func newIdenticalFailer(t *testing.T) *identicalFailer {
	t.Helper()
	return &identicalFailer{exit: exitError(t, 3)}
}

func (f *identicalFailer) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	f.calls++
	return nil, &model.ToolExecutionError{Cause: &model.ToolCommandFailure{
		NodeID:  node.NodeID(),
		Outcome: "shell command failed",
		Cause:   f.exit,
		Stderr:  "x",
	}}
}

// retryOnce is the catch-all recipe without its wait: one retry of an
// EXECUTION_FAILED, then a terminal failure.
func retryOnce(_ context.Context, err error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
	code := ClassifyError(err)
	if prior(code) >= 1 {
		return RecoveryAction{Kind: RecoveryFailTerminal}, code
	}
	return RecoveryAction{Kind: RecoveryRetrySameNode}, code
}

func soloNodeWorkflow(node ir.Node) *ir.Workflow {
	return &ir.Workflow{
		Name:  "identical",
		Entry: node.NodeID(),
		Nodes: map[string]ir.Node{node.NodeID(): node, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Edges: []*ir.Edge{{From: node.NodeID(), To: "done"}},
	}
}

// The scope, through the engine: an identical repeat is read from a tool
// node's command alone. An agent that fails twice with the same words, a
// registry tool, and a Verified Action whose ladder asks a model keep the
// code they had — a model's next sample, or a remote call's next answer, can
// differ however alike two errors read.
func TestAnIdenticalRepeatIsReadFromAToolCommandOnly(t *testing.T) {
	base := ir.BaseNode{ID: "n"}
	for _, c := range []struct {
		name string
		node ir.Node
		want ErrorCode
	}{
		{"a shell command", &ir.ToolNode{BaseNode: base, Command: "make test"}, ErrCodeToolFailedPermanent},
		{"an agent", &ir.AgentNode{BaseNode: base}, ErrCodeExecutionFailed},
		{"a judge", &ir.JudgeNode{BaseNode: base}, ErrCodeExecutionFailed},
		{"a registry tool", &ir.ToolNode{BaseNode: base, Command: "read_file"}, ErrCodeExecutionFailed},
		{"a verified action with a self-repair rung", &ir.ToolNode{BaseNode: base, Command: "make test", Postcondition: "test -f ok",
			Policy: ir.PolicyRecover, Recovery: &ir.RecoverySpec{MaxRepairAttempts: 1}}, ErrCodeExecutionFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			pinScenarioLimit(t)
			exec := newIdenticalFailer(t)
			s := tmpStore(t)
			err := New(soloNodeWorkflow(c.node), s, exec, WithRecoveryDispatch(retryOnce)).Run(context.Background(), "run-scope", nil)
			var rt *RuntimeError
			if !errors.As(err, &rt) {
				t.Fatalf("the engine returned no RuntimeError: %v", err)
			}
			if rt.Code != c.want {
				t.Errorf("code = %s, want %s", rt.Code, c.want)
			}
			if exec.calls != scenarioLimit {
				t.Errorf("the node ran %d time(s), want %d", exec.calls, scenarioLimit)
			}
			r, lerr := s.LoadRun(context.Background(), "run-scope")
			if lerr != nil {
				t.Fatal(lerr)
			}
			_, counted := r.Checkpoint.NodeFailureStreaks["n"]
			if wantCounted := c.want == ErrCodeToolFailedPermanent; counted != wantCounted {
				t.Errorf("a streak in the checkpoint = %v, want %v (%+v)", counted, wantCounted, r.Checkpoint.NodeFailureStreaks)
			}
		})
	}
}

// A recipe that pauses for a human is the host's own answer to the failure,
// and a pause is never redelivered: the repeat does not replace it.
func TestAnIdenticalRepeatLeavesARecoveryPauseAlone(t *testing.T) {
	pinScenarioLimit(t)
	exec := newIdenticalFailer(t)
	s := tmpStore(t)
	pauseOnSecond := func(_ context.Context, err error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		code := ClassifyError(err)
		if prior(code) >= 1 {
			return RecoveryAction{Kind: RecoveryPauseForHuman, Reason: "ask the operator"}, code
		}
		return RecoveryAction{Kind: RecoveryRetrySameNode}, code
	}
	wf := soloNodeWorkflow(&ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}, Command: "make test"})
	err := New(wf, s, exec, WithRecoveryDispatch(pauseOnSecond)).Run(context.Background(), "run-pause", nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("err = %v, want the recipe's pause", err)
	}
	r, lerr := s.LoadRun(context.Background(), "run-pause")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("status = %s, want %s", r.Status, store.RunStatusPausedWaitingHuman)
	}
}

// Which failures of a tool command are counted: the command ran to an exit
// status of its own, on a node an identical repeat is read from. A failure
// with no status in its chain is one where the command never ran — the
// script file could not be written, with a name the executor draws afresh
// each time — and a status beside sandbox.ErrExecStatusUnknown is the exec
// client's, not the command's.
func TestOnlyACommandThatRanToItsOwnExitStatusIsCounted(t *testing.T) {
	shell := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: "make test"}
	ran := shellFailure(t, 3, "x")
	neverWritten := func(name string) error {
		return &model.ToolExecutionError{Cause: fmt.Errorf("model: tool node %q: create temp script: open /tmp/%s: permission denied", "gate", name)}
	}
	unknownEnd := &model.ToolExecutionError{Cause: fmt.Errorf("model: tool node %q: shell command failed: %w\nstdout: \nstderr: error: unable to upgrade connection: container not found (\"workload\")",
		"gate", errors.Join(exitError(t, 1), &sandbox.ExecStatusUnknownError{Swept: true, Detail: "the sweep found nothing left running"}))}
	for _, c := range []struct {
		name string
		node ir.Node
		err  error
		want bool
	}{
		{"a command that exited 3", shell, ran, true},
		{"the same failure on an agent", &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}}, ran, false},
		{"a script file that could not be written", shell, neverWritten(".iterion-script-1234567890.sh"), false},
		{"a command whose end was never proven", shell, unknownEnd, false},
		{"an outage, in curl's words", shell, shellFailure(t, 6, "curl: (6) Could not resolve host: api.github.com"), false},
		{"an error that is no tool failure", shell, errors.New("exit status 3"), false},
	} {
		_, _, got := countedToolFailure(c.node, ClassifyError(c.err), c.err)
		if got != c.want {
			t.Errorf("%s: counted = %v, want %v (classified %s)", c.name, got, c.want, ClassifyError(c.err))
		}
	}
	// The two temp names read alike once normalised: the status, not the
	// text, is what keeps that failure out of a streak.
	a, b := neverWritten(".iterion-script-1234567890.sh"), neverWritten(".iterion-script-42.sh")
	var fa, fb *model.ToolExecutionError
	if !errors.As(a, &fa) || !errors.As(b, &fb) || fa.StableText() != fb.StableText() {
		t.Fatal("scenario broken: the two setup failures do not read alike once the temp name is normalised")
	}
	if _, ok := fa.ExitStatus(); ok {
		t.Error("a script file that could not be written carries an exit status")
	}
}

// sequenceFailer fails call i with errs[i], the last one repeated.
type sequenceFailer struct {
	calls int
	errs  []error
}

func (f *sequenceFailer) Execute(_ context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
	i := min(f.calls, len(f.errs)-1)
	f.calls++
	return nil, f.errs[i]
}

// The count is of failures IN A ROW. A failure of another kind between two
// identical ones — here a network failure, which keeps its own code — ends
// the streak: the third failure is a first one again, not a repeat.
func TestAFailureOfAnotherKindBetweenTwoIdenticalOnesEndsTheStreak(t *testing.T) {
	pinScenarioLimit(t)
	x := shellFailure(t, 1, "x")
	network := shellFailure(t, 7, "curl: (7) Failed to connect: Connection refused")
	if got := ClassifyError(network); got != ErrCodeNetworkTransient {
		t.Fatalf("scenario broken: the network failure is classified %s", got)
	}
	exec := &sequenceFailer{errs: []error{x, network, x}}
	s := tmpStore(t)
	wf := soloNodeWorkflow(&ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}, Command: "make test"})
	// retryOnce counts per code: x is retried once, the network failure once,
	// and the second x is terminal.
	err := New(wf, s, exec, WithRecoveryDispatch(retryOnce)).Run(context.Background(), "run-between", nil)
	var rt *RuntimeError
	if !errors.As(err, &rt) {
		t.Fatalf("the engine returned no RuntimeError: %v", err)
	}
	if exec.calls != 3 {
		t.Fatalf("scenario broken: the node ran %d time(s), want 3", exec.calls)
	}
	if rt.Code != ErrCodeExecutionFailed {
		t.Errorf("x, a network failure, x ended %s, want %s — the failure in between did not end the streak", rt.Code, ErrCodeExecutionFailed)
	}
	r, lerr := s.LoadRun(context.Background(), "run-between")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if streak := r.Checkpoint.NodeFailureStreaks["n"]; streak.Count != 1 {
		t.Errorf("checkpoint streak = %+v, want a count of 1: the second x starts its own", streak)
	}
}

// A command whose end the sandbox never proved, and one that never ran, are
// not "the same exit status and output" however alike two attempts read: the
// run keeps EXECUTION_FAILED, and no streak is written.
func TestAFailureWithNoProvenExitStatusIsNeverPromoted(t *testing.T) {
	for name, failure := range map[string]error{
		"the sandbox exec session ended without the command's completion line": &model.ToolExecutionError{Cause: fmt.Errorf("model: tool node %q: shell command failed: %w\nstdout: \nstderr: error: unable to upgrade connection: container not found (\"workload\")",
			"n", errors.Join(exitError(t, 1), &sandbox.ExecStatusUnknownError{Swept: true, Detail: "the sweep found nothing left running"}))},
		"the script file could not be written": &model.ToolExecutionError{Cause: fmt.Errorf("model: tool node %q: create temp script: open /tmp/.iterion-script-77.sh: permission denied", "n")},
	} {
		t.Run(name, func(t *testing.T) {
			pinScenarioLimit(t)
			if got := ClassifyError(failure); got != ErrCodeExecutionFailed {
				t.Fatalf("scenario broken: the failure is classified %s", got)
			}
			exec := &sequenceFailer{errs: []error{failure}}
			s := tmpStore(t)
			wf := soloNodeWorkflow(&ir.ToolNode{BaseNode: ir.BaseNode{ID: "n"}, Command: "make test"})
			err := New(wf, s, exec, WithRecoveryDispatch(retryOnce)).Run(context.Background(), "run-unproven", nil)
			var rt *RuntimeError
			if !errors.As(err, &rt) {
				t.Fatalf("the engine returned no RuntimeError: %v", err)
			}
			if rt.Code != ErrCodeExecutionFailed || exec.calls != scenarioLimit {
				t.Errorf("code = %s after %d attempt(s), want %s after %d", rt.Code, exec.calls, ErrCodeExecutionFailed, scenarioLimit)
			}
			r, lerr := s.LoadRun(context.Background(), "run-unproven")
			if lerr != nil {
				t.Fatal(lerr)
			}
			if streak, counted := r.Checkpoint.NodeFailureStreaks["n"]; counted {
				t.Errorf("the failure was counted in a streak: %+v", streak)
			}
		})
	}
}
