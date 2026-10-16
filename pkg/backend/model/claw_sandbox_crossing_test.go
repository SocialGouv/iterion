package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// loggedTestClawBackend is newTestClawBackend plus the operator-facing log,
// so a witness can assert the gap the backend NAMES and not only the
// behaviour it keeps.
func loggedTestClawBackend(logs *strings.Builder) *ClawBackend {
	return NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelInfo, logs)))
}

// sandboxDrainBound is how long a witness waits for a dispatch whose only
// remaining obstacle is the pipe drain. Comfortably over
// clawRunnerWaitDelay, and short enough that an unbounded Wait reads as a
// failed assertion instead of a package timeout.
const sandboxDrainBound = 20 * time.Second

// newPipeHoldingSandbox is a runner that leaves a descendant holding the
// pipes this process drains — what a sandbox driver does whenever the
// command it starts spawns anything. The holder lives exactly as long as the
// test: it polls a file under the test's own temp dir, which the framework
// removes on cleanup, so a witness that times out does not leak it and a
// witness that passes is not passing because the holder went away.
type pipeHoldingSandbox struct {
	hold  string
	ready string
	arg   string
	tail  string
}

func newPipeHoldingSandbox(t *testing.T, dir, tail, arg string) *pipeHoldingSandbox {
	t.Helper()
	s := &pipeHoldingSandbox{
		hold:  filepath.Join(dir, "hold"),
		ready: filepath.Join(dir, "ready"),
		arg:   arg,
		tail:  tail,
	}
	if err := os.WriteFile(s.hold, []byte("held"), 0o600); err != nil {
		t.Fatalf("arm the pipe holder: %v", err)
	}
	return s
}

func (s *pipeHoldingSandbox) Driver() string { return "pipe-holding" }
func (s *pipeHoldingSandbox) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	return scriptedSpawn(ctx, argv, func() *exec.Cmd {
		script := "while [ -e \"$1\" ]; do sleep 1; done &\n: > \"$2\"\n" + s.tail
		return exec.CommandContext(ctx, "sh", "-c", script, "sh", s.hold, s.ready, s.arg)
	})
}
func (s *pipeHoldingSandbox) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (s *pipeHoldingSandbox) Cleanup(context.Context) error { return nil }

func awaitSandboxReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(sandboxDrainBound)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the sandbox double never signalled readiness at %s", path)
}

// cmd.Wait blocks on the goroutine copying the runner's stderr, and that
// copier only ends when EVERY holder of the pipe closes it. A descendant the
// runner left behind is such a holder, so Wait has to be bounded or the node
// parks forever — unbounded, this dispatch never returns even though its
// result envelope has already been read. The bound expiring is NOT the
// node's failure: the answer arrived, only the drain was abandoned.
func TestSandboxDispatch_AnswersWhenTheRunnerLeavesItsPipesHeld(t *testing.T) {
	dir := t.TempDir()
	resEnv, err := delegate.NewResultEnvelope(delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(resEnv)
	if err != nil {
		t.Fatal(err)
	}
	resultFile := filepath.Join(dir, "result.ndjson")
	if err := os.WriteFile(resultFile, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	b := loggedTestClawBackend(&logs)
	type outcome struct {
		res delegate.Result
		err error
	}
	run := newPipeHoldingSandbox(t, dir, awaitTaskEnvelope+"cat \"$3\"\nexit 0", resultFile)
	done := make(chan outcome, 1)
	go func() {
		res, derr := b.executeViaSandboxRunner(context.Background(), delegate.Task{
			NodeID: "n", Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		})
		done <- outcome{res, derr}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the node was failed by the abandoned drain: %v — the runner answered and exited 0, only its pipes outlived it", got.err)
		}
		if got.res.Output["verdict"] != "ok" {
			t.Fatalf("output = %v, want the runner's answer", got.res.Output)
		}
		// Not charged to the node, but not hidden either: the drain was
		// abandoned, and that is a fact about the container's processes.
		if !strings.Contains(logs.String(), "left its pipes held") {
			t.Errorf("the abandoned drain is unreported: %q", logs.String())
		}
		// This runner wrote nothing on stderr, and the report stays silent
		// for it: the operator's log is not given a line per crossing.
		if strings.Contains(logs.String(), "the sandbox runner's stderr") {
			t.Errorf("a healthy crossing's empty stderr was reported anyway: %q", logs.String())
		}
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the dispatch did not return in %s — cmd.Wait is parked on a pipe a descendant of the exited runner still holds", sandboxDrainBound)
	}
}

// The same bound on the cancellation path: when the run is cancelled, the
// engine must get the node back. The driver kills the runner it started; a
// descendant it does not know about keeps the pipes open, so the unbounded
// Wait outlives the cancellation it was supposed to honour.
//
// What the returned error says ABOUT the cut is not this witness's subject:
// a cancelled crossing is observed as an I/O error on a pipe the bound
// closed, and carrying the cancellation itself out is #2456.
func TestSandboxDispatch_ReturnsOnCancelWhenThePipesAreHeld(t *testing.T) {
	dir := t.TempDir()
	run := newPipeHoldingSandbox(t, dir, "while [ -e \"$1\" ]; do sleep 1; done", "")
	b := newTestClawBackend()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.executeViaSandboxRunner(ctx, delegate.Task{
			NodeID: "n", Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		})
		done <- err
	}()
	awaitSandboxReady(t, run.ready)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled dispatch reported success")
		}
		// The cut a cancelled crossing reports is terminal, and stays so:
		// the run is going down regardless, and a transient reading would
		// spend a retry budget on a context that is already done.
		if isDelegateRetryable(err) {
			t.Fatalf("a cancelled crossing's cut classified as transient: %v", err)
		}
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the dispatch did not return in %s of the cancel — cmd.Wait is parked on a pipe the killed runner's descendant still holds", sandboxDrainBound)
	}
}

// uncancelledPipeHoldingSandbox is a driver whose Cmd carries no Cancel —
// what [os/exec] documents as a helper deliberately letting the command
// outlive the cancellation of its context. Then WaitDelay is the ONLY
// reason Start arms the context watcher, so the bound has to be in place
// BEFORE Start or a cancelled crossing has no bound at all. The shipped
// drivers all build their Cmd with exec.CommandContext, whose Cancel arms
// that watcher on its own — which is why the sibling cancellation witness
// cannot see the ordering and this one is its own test.
type uncancelledPipeHoldingSandbox struct {
	*pipeHoldingSandbox
}

func (s uncancelledPipeHoldingSandbox) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	c := s.pipeHoldingSandbox.Command(ctx, cmd, opts)
	c.Cancel = nil
	return c
}

func TestSandboxDispatch_BoundsTheCancelOfADriverThatDoesNotKill(t *testing.T) {
	dir := t.TempDir()
	held := newPipeHoldingSandbox(t, dir, "while [ -e \"$1\" ]; do sleep 1; done", "")
	run := uncancelledPipeHoldingSandbox{held}
	b := newTestClawBackend()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.executeViaSandboxRunner(ctx, delegate.Task{
			NodeID: "n", Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		})
		done <- err
	}()
	awaitSandboxReady(t, held.ready)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled dispatch reported success")
		}
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the dispatch did not return in %s of the cancel — the runner's Cmd was started with WaitDelay still zero, so exec armed no context watcher and nothing bounds this wait", sandboxDrainBound)
	}
}

// The bound is on the orphan-pipe drain, and its clock starts when Wait
// observes the runner's exit or when the context is done — never while the
// runner is working. A crossing slower than clawRunnerWaitDelay therefore
// answers normally; a bound that cut live work instead would cap every
// sandboxed node at two seconds.
func TestSandboxDispatch_TheBoundDoesNotCutARunnerStillWorking(t *testing.T) {
	dir := t.TempDir()
	resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	// Past the bound, and still a fraction of sandboxDrainBound. Derived from
	// the constant, so shortening the bound does not make this vacuous.
	slower := clawRunnerWaitDelay + time.Second

	var logs strings.Builder
	b := loggedTestClawBackend(&logs)
	start := time.Now()
	res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: &scriptedSandbox{
			script: awaitTaskEnvelope + "sleep \"$1\"\ncat \"$2\"\nexit 0",
			args:   []string{strconv.Itoa(int(slower / time.Second)), resultFile},
		},
	})
	if err != nil {
		t.Fatalf("a runner slower than the %s bound was cut: %v", clawRunnerWaitDelay, err)
	}
	if res.Output["verdict"] != "ok" {
		t.Fatalf("output = %v, want the runner's answer", res.Output)
	}
	if elapsed := time.Since(start); elapsed < slower {
		t.Fatalf("the crossing returned in %s, under the %s this runner sleeps — this witness no longer drives a runner slower than the bound", elapsed, slower)
	}
}

// The runner's stderr is its ONE channel for a diagnostic that does not
// fail the node: a replay it could not decode, a relayed event too large to
// ship, a settings-hooks notice. The launcher folds that buffer into the
// node's error on the paths that have one — so on the paths that do not, the
// operator has to be told anyway, or the diagnostic is written to nobody.
func TestSandboxDispatch_ReportsTheRunnersStderrWhenTheNodeAnswers(t *testing.T) {
	dir := t.TempDir()
	resEnv, err := delegate.NewResultEnvelope(delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(resEnv)
	if err != nil {
		t.Fatal(err)
	}
	resultFile := filepath.Join(dir, "result.ndjson")
	if err := os.WriteFile(resultFile, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	const warning = "iterion-claw-runner: warn: decode session_replay: unexpected end of JSON input"
	var logs strings.Builder
	b := loggedTestClawBackend(&logs)
	res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: newAnsweringSandbox(t, warning, resultFile),
	})
	if err != nil {
		t.Fatalf("the node was failed by a diagnostic it answered through: %v", err)
	}
	if res.Output["verdict"] != "ok" {
		t.Fatalf("output = %v, want the runner's answer", res.Output)
	}
	if !strings.Contains(logs.String(), warning) {
		t.Errorf("the runner's stderr is nowhere in the operator-facing log: %q", logs.String())
	}
}

// newAnsweringSandbox is a runner that writes one line to stderr, emits a
// pre-built result envelope and exits 0 — the success path, with a
// diagnostic on the only channel it has for one.
func newAnsweringSandbox(t *testing.T, stderrLine, resultFile string) *scriptedSandbox {
	t.Helper()
	return &scriptedSandbox{
		script: awaitTaskEnvelope + "printf '%s\\n' \"$1\" >&2\ncat \"$2\"\nexit 0",
		args:   []string{stderrLine, resultFile},
	}
}

// awaitTaskEnvelope is the first line of any scripted runner that EXITS on its
// own. The launcher writes the task envelope after Start, so a runner that
// exits first closes the read end under it and the dispatch takes the
// pre-task abort (`send task envelope: … broken pipe`) instead of the outcome
// the witness drives — a race the launcher usually wins and `-race` usually
// loses. Reading the envelope first makes the order a fact. A read that fails
// (stdin already closed) falls through rather than hanging.
const awaitTaskEnvelope = "IFS= read -r _\n"

// What a runner writes on stderr is the container's log: its own newlines and
// its own idea of how much to write. The report is ONE record — a block whose
// body lines all carry the logger's continuation prefix, never one prefixed
// line followed by thousands of bare ones in the run log — and the bound is on
// what the operator's log RECEIVES, so a runaway buffer cannot take it with it.
//
// Driven at both ends of the shape the container chooses: single-byte lines,
// where the prefix is 13 of every 14 bytes written (a progress spinner, a
// per-token trace), and 1000-byte lines, where it is noise. Both over the cap
// by more than the cap itself, so a dropped bound overshoots it by megabytes
// rather than by the rounding of one line.
func TestSandboxDispatch_ReportsTheRunnersStderrAsOneBoundedBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write string
	}{
		{"single-byte lines", "yes d | head -n 3000000 >&2\n"},
		{"1000-byte lines", "i=0\nwhile [ $i -lt 2200 ]; do printf '%s\\n' \"$1\" >&2; i=$((i+1)); done\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
			var logs strings.Builder
			b := loggedTestClawBackend(&logs)
			script := awaitTaskEnvelope + tc.write + "cat \"$2\"\nexit 0"
			res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
				NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
				Sandbox: &scriptedSandbox{script: script, args: []string{strings.Repeat("d", 1000), resultFile}},
			})
			if err != nil {
				t.Fatalf("the crossing failed: %v", err)
			}
			if res.Output["verdict"] != "ok" {
				t.Fatalf("output = %v, want the runner's answer", res.Output)
			}
			out := logs.String()
			if n := strings.Count(out, "the sandbox runner's stderr:"); n != 1 {
				t.Fatalf("the report is %d records, want exactly one", n)
			}
			// The header line is the host's; the prefixed lines under it are
			// the container's, and those are what the bound covers. Measured
			// on the report alone: charging the whole log would make any
			// other record of the same dispatch — the host-forfait notice,
			// an abandoned drain — read as a broken bound.
			body := stderrReportBody(t, out)
			if !strings.Contains(body, "...[truncated]") {
				t.Errorf("an over-cap stderr buffer went to the operator's log whole (%d bytes)", len(body))
			}
			if len(body) > maxFieldSize {
				t.Errorf("one crossing wrote %d bytes of container log to the operator's log, over the %d-byte bound", len(body), maxFieldSize)
			}
		})
	}
}

// stderrReportBody returns the bytes the operator's log received for the
// stderr report's BODY: the continuation-prefixed lines under the report's
// header, each with the newline the renderer puts back — exactly what
// [iterlog.BlockPreviewRendered]'s bound is expressed in.
//
// The body ends at the first line carrying no prefix, and that line must open
// another record (the human format starts every one with "HH:MM:SS "): a body
// line loose in the run log is the defect this reading would otherwise hide.
func stderrReportBody(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, "the sandbox runner's stderr:") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no stderr report in the operator's log: %.200q", out)
	}
	var body strings.Builder
	for i, line := range lines[start+1:] {
		if !strings.HasPrefix(line, "  ") {
			if len(line) < 9 || line[2] != ':' || line[5] != ':' || line[8] != ' ' {
				t.Fatalf("line %d of the report carries no block prefix — the body is loose in the run log: %.80q", start+1+i, line)
			}
			break
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	return body.String()
}

// The report's level follows the outcome. The operator's default log carries
// the container's words either way, but a node that ANSWERED does not raise a
// warning on its container's log, and a crossing that FAILED does.
func TestSandboxDispatch_TheStderrReportTakesTheOutcomesLevel(t *testing.T) {
	const notice = "iterion-claw-runner: warn: settings hooks: 1 entry ignored"
	dir := t.TempDir()
	resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})

	var answered strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelWarn, &answered)))
	if _, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: newAnsweringSandbox(t, notice, resultFile),
	}); err != nil {
		t.Fatalf("the crossing failed: %v", err)
	}
	if strings.Contains(answered.String(), "the sandbox runner's stderr") {
		t.Errorf("a node that answered raised a warning on its container's log: %q", answered.String())
	}

	// The same buffer, on a crossing that fails: a warning the operator's
	// default log level keeps.
	var failed strings.Builder
	b2 := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
		WithClawLogger(iterlog.New(iterlog.LevelWarn, &failed)))
	if _, err := b2.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: &scriptedSandbox{
			script: awaitTaskEnvelope + "printf '%s\\n' \"$1\" >&2\nexit 7",
			args:   []string{notice},
		},
	}); err == nil {
		t.Fatal("a runner that exited 7 without answering reported success")
	}
	if !strings.Contains(failed.String(), notice) {
		t.Errorf("a failed crossing's stderr is below the operator's default level: %q", failed.String())
	}
}

// The buffer crosses the run's redaction ONCE, for every reader. The pass is
// the run's secret guard over the whole text — an unmaterialize, a heuristic
// detector and a recursive decode — so a second read is a second scan of the
// container's entire log, on the hot path of every sandboxed node.
//
// A crossing that FAILS is where the readers are plural: its error quotes the
// buffer and the deferred report prints it. The answering case below is the
// single-reader half of the same claim, and only the failing one can see a
// pass computed per reader.
func TestSandboxDispatch_RedactsTheRunnersStderrOncePerCrossing(t *testing.T) {
	const width = 10_000
	for _, tc := range []struct {
		name    string
		mk      func(t *testing.T, dir, stderrLine string) *scriptedSandbox
		wantErr bool
	}{
		{
			name: "a node that answers — the report is the only reader",
			mk: func(t *testing.T, dir, stderrLine string) *scriptedSandbox {
				res := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
				return newAnsweringSandbox(t, stderrLine, res)
			},
		},
		{
			name: "a runner that dies without answering — the error quotes the buffer too",
			mk: func(t *testing.T, dir, stderrLine string) *scriptedSandbox {
				return &scriptedSandbox{
					script: awaitTaskEnvelope + "printf '%s\\n' \"$1\" >&2\nexit 7",
					args:   []string{stderrLine},
				}
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var scanned atomic.Int64
			var logs strings.Builder
			b := loggedTestClawBackend(&logs)
			_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
				NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
				Sandbox: tc.mk(t, dir, strings.Repeat("d", width)),
				RedactSecrets: func(s string) string {
					scanned.Add(int64(len(s)))
					return s
				},
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("outcome err = %v, want error: %v — this case no longer drives the readers it counts", err, tc.wantErr)
			}
			if !strings.Contains(logs.String(), strings.Repeat("d", 100)) {
				t.Fatalf("the buffer never reached the report — nothing was counted: %.200q", logs.String())
			}
			// The buffer is the line plus its newline; the empty
			// IOResult.Error the error branch also passes adds nothing.
			if got := scanned.Load(); got != width+1 {
				t.Errorf("the run's redaction scanned %d bytes of a %d-byte stderr buffer", got, width+1)
			}
		})
	}
}

// And a report nobody will read costs nothing. The same pass over the same
// buffer is pure waste on the two silent paths — a backend with no operator
// log, and an operator log above the level a healthy crossing reports at —
// because the record is dropped on its level before anything is printed.
// Those are the paths where the buffer is the container's whole log and no
// error branch reads it either.
func TestSandboxDispatch_SkipsTheStderrPassWhenNoSinkWillKeepTheReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(w *strings.Builder) *ClawBackend
	}{
		{
			name: "no operator log at all",
			mk:   func(*strings.Builder) *ClawBackend { return newTestClawBackend() },
		},
		{
			name: "an operator log above the report's level",
			mk: func(w *strings.Builder) *ClawBackend {
				return NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{},
					WithClawLogger(iterlog.New(iterlog.LevelWarn, w)))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
			var scanned atomic.Int64
			var logs strings.Builder
			res, err := tc.mk(&logs).executeViaSandboxRunner(context.Background(), delegate.Task{
				NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
				Sandbox: newAnsweringSandbox(t, strings.Repeat("d", 10_000), resultFile),
				RedactSecrets: func(s string) string {
					scanned.Add(int64(len(s)))
					return s
				},
			})
			if err != nil {
				t.Fatalf("the crossing failed: %v", err)
			}
			if res.Output["verdict"] != "ok" {
				t.Fatalf("output = %v, want the runner's answer", res.Output)
			}
			if got := scanned.Load(); got != 0 {
				t.Errorf("the run's redaction scanned %d bytes for a report no sink keeps", got)
			}
		})
	}
}

// The one wait no bound covers: a runner that dies WITHOUT writing its result
// envelope while a descendant holds the pipe leaves the multiplexer's read of
// stdout with nothing to end it — no EOF, and cmd.Wait not yet reached. What
// recovers that node is the crossing's own reaper, clawRunnerWaitDelay +
// clawPipeDrainGrace after the kill: the WaitDelay ends the runner's Wait,
// the grace closes the owned pipe ends the read sits on, and the dispatch
// then returns. The cancellation itself closes nothing this process owns.
// This
// witness is the doc's claim (docs/sandbox.md, rule 1) under execution:
// without that recovery the dispatch parks until the package times out.
// What the error it returns says about the cut is #2456's subject, not this
// witness's.
func TestSandboxDispatch_ACancelRecoversACrossingWhoseRunnerNeverAnswered(t *testing.T) {
	dir := t.TempDir()
	run := newPipeHoldingSandbox(t, dir, awaitTaskEnvelope+"exit 0", "")
	b := newTestClawBackend()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.executeViaSandboxRunner(ctx, delegate.Task{
			NodeID: "n", Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		})
		done <- err
	}()
	awaitSandboxReady(t, run.ready)
	// The readiness file is written before the runner reads its task
	// envelope and exits: give it that much, so the cancel lands on the
	// parked read and not on a live runner.
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the crossing returned before the cancel (%v) — the parked read this witness watches no longer happens", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled dispatch reported success")
		}
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the cancel did not recover the parked read in %s", sandboxDrainBound)
	}
}

// The multiplexer's read of the runner's stdout comes BEFORE cmd.Wait, so
// clawRunnerWaitDelay does not bound it: a runner that dies without writing
// its result envelope while a descendant holds the pipe leaves that read
// with nothing to end it — no EOF (a writer still lives) and no Wait in
// sight. Unowned, the dispatch parks until the run's cancellation recovers
// the node, on every crossing of the shape. Owned, the pipe ends the read
// without a cancel: Wait returns clawRunnerWaitDelay after the runner's
// exit, and the read end's reaper unblocks the parked read one
// clawPipeDrainGrace later.
func TestSandboxDispatch_TheHeldStdoutReadEndsWithoutACancel(t *testing.T) {
	dir := t.TempDir()
	run := newPipeHoldingSandbox(t, dir, awaitTaskEnvelope+"exit 0", "")
	b := newTestClawBackend()
	done := make(chan error, 1)
	go func() {
		_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
			NodeID: "n", Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		})
		done <- err
	}()
	awaitSandboxReady(t, run.ready)
	// The readiness file is written before the runner reads its task
	// envelope and exits: give it that much, so the shape is the parked read
	// and not a runner still setting up.
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a runner that died without its result envelope reported success")
		}
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the dispatch did not return in %s and nothing cancelled the run — the multiplexer's read of the held stdout pipe is unbounded", sandboxDrainBound)
	}
}

// The grace is what keeps an answer the runner ALREADY WROTE alive while the
// launcher is still inside a slow envelope handler: a pipe's buffered bytes
// die with its read end, so a close that runs AT the Wait — the naive bound
// of the parked read, measured losing the answer 3 runs out of 3 — drops
// whatever still sits in the PIPE behind a handler that outlasts the
// runner's exit. The crossing closes the read end one clawPipeDrainGrace
// AFTER the Wait instead. The handler here runs past clawRunnerWaitDelay, so
// the at-Wait close lands first; it is well inside the grace, so the real
// close does not. Two subtleties keep the shape honest: the answer is an
// EVENT envelope — fire-and-forget, no reply — because the runner here is a
// shell, and a shell starts a background job with /dev/null for its stdin
// (POSIX), so once the runner has exited no descendant of it holds the
// pipe's read end and the launcher's reply fails with the broken pipe that
// is the truth about that crossing, whatever the owned pipes do; and the
// runner writes the
// answer a beat AFTER the envelope the handler is busy with, so the answer
// sits in the pipe and not in the reader's own scan buffer, where no close
// could reach it. The runner's descendant holds the pipe, so the shape is a
// held-pipe crossing end to end.
func TestSandboxDispatch_TheGraceKeepsABufferedAnswerAlivePastASlowHandler(t *testing.T) {
	dir := t.TempDir()
	// Past the at-Wait close (clawRunnerWaitDelay after the runner's exit),
	// inside the grace that follows it. Derived from the constant, so
	// shortening the bound does not make this vacuous.
	slower := clawRunnerWaitDelay + time.Second

	eventEnv, err := delegate.NewEventEnvelope(string(store.EventLLMRequest), map[string]any{"model": "openai/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	resEnv, err := delegate.NewResultEnvelope(delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	var says strings.Builder
	for _, env := range []delegate.Envelope{eventEnv, resEnv} {
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		says.Write(raw)
		says.WriteByte('\n')
	}
	saysPath := filepath.Join(dir, "runner-says.ndjson")
	if err := os.WriteFile(saysPath, []byte(says.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	slow := false
	b := NewClawBackend(NewRegistry(), EventHooks{
		OnLLMRequest: func(string, LLMRequestInfo) {
			time.Sleep(slower)
			slow = true
		},
	}, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelInfo, &logs)))
	// Line 1 goes out at once (the handler picks it up); line 2 — the answer
	// — goes out 200 ms later, while the handler is mid-sleep, and stays in
	// the pipe until the handler returns.
	run := newPipeHoldingSandbox(t, dir,
		awaitTaskEnvelope+`head -n 1 "$3"`+"\nsleep 0.2\n"+`tail -n 1 "$3"`+"\nexit 0", saysPath)
	res, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
	})
	if err != nil {
		t.Fatalf("the answer buffered behind the slow handler was lost: %v", err)
	}
	if !slow {
		t.Fatal("the slow handler never ran — this witness no longer drives the shape it guards")
	}
	if res.Output["verdict"] != "ok" {
		t.Fatalf("output = %v, want the runner's answer", res.Output)
	}
}

// A bound cuts, and this one can cut an answer the runner had ALREADY
// written: a handler that outlasts clawPipeDrainGrace past the Wait loses
// whatever the pipe still buffers behind it, and the worst case is a runner
// that exits CLEANLY — no descendant delaying the Wait, so the whole budget
// is the grace itself. The loss is the trade the grace buys, and the failure
// it produces owes the truth about itself: the drain bound named as what
// ended the read (not an internal pipe bug), the closed-pipe cause and the
// runner's stderr readable through it, and a transient classification — the
// runner exited 0, and the run recovers by re-running the node. A cancelled
// crossing is the counter-shape the naming must never swallow: there the
// run's context is done, the cut keeps the plain multiplexer error and stays
// terminal (the sibling cancel witnesses hold that), and the closed-pipe
// prose alone — no reaper behind it — classifies as nothing.
func TestSandboxDispatch_AnswerDroppedPastTheGraceIsNamedAndRetried(t *testing.T) {
	dir := t.TempDir()
	// A full grace plus slack, past a runner that exits cleanly. Derived
	// from the constant, so shortening the bound does not make this
	// vacuous — and lengthening it does not hide the loss either: the
	// handler is timed from the envelope the launcher reads first.
	slower := clawPipeDrainGrace + time.Second

	eventEnv, err := delegate.NewEventEnvelope(string(store.EventLLMRequest), map[string]any{"model": "openai/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	resEnv, err := delegate.NewResultEnvelope(delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	var says strings.Builder
	for _, env := range []delegate.Envelope{eventEnv, resEnv} {
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		says.Write(raw)
		says.WriteByte('\n')
	}
	saysPath := filepath.Join(dir, "runner-says.ndjson")
	if err := os.WriteFile(saysPath, []byte(says.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	slow := false
	b := NewClawBackend(NewRegistry(), EventHooks{
		OnLLMRequest: func(string, LLMRequestInfo) {
			time.Sleep(slower)
			slow = true
		},
	}, RetryPolicy{})
	// A scripted runner, not the pipe-holding one: nothing delays the
	// Wait, so the cut lands a bare grace after the runner's exit — the
	// smallest budget the bound ever gets, and the shape the witness must
	// hold.
	run := &scriptedSandbox{
		script: awaitTaskEnvelope + `head -n 1 "$1"` + "\nsleep 0.2\n" + `tail -n 1 "$1"` + "\nexit 0",
		args:   []string{saysPath},
	}
	_, err = b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
	})
	if !slow {
		t.Fatal("the slow handler never ran — this witness no longer drives the shape it guards")
	}
	if err == nil {
		t.Fatal("the dispatch succeeded past the grace — the drain bound no longer cuts, and this witness no longer drives the trade")
	}
	// The loss is real: the runner WROTE the answer the dispatch failed.
	saysBack, saysErr := os.ReadFile(saysPath)
	if saysErr != nil {
		t.Fatal(saysErr)
	}
	if !strings.Contains(string(saysBack), `"verdict":"ok"`) {
		t.Fatalf("the shape is wrong — the runner never wrote the answer this witness guards: %s", saysBack)
	}
	// The mechanism, not an internal pipe bug.
	if !strings.Contains(err.Error(), "drain bound") || !strings.Contains(err.Error(), clawPipeDrainGrace.String()) {
		t.Fatalf("the dropped answer failed without naming the drain bound: %v", err)
	}
	// The operation named is the one that hit the close: this shape's cut is
	// the multiplexer's read of stdout (the reply-write shape has its own
	// witness).
	if !strings.Contains(err.Error(), "the multiplexer's read ended") {
		t.Fatalf("the read cut is not named as a read: %v", err)
	}
	// The cause and the container's account stay readable through it.
	if !strings.Contains(err.Error(), "file already closed") || !strings.Contains(err.Error(), "(stderr: ") {
		t.Fatalf("the cut's cause or the runner's stderr is not readable through the error: %v", err)
	}
	// Transient: the runner exited 0, and re-running the node recovers what
	// the run a delivered answer cannot.
	if !isDelegateRetryable(err) {
		t.Fatalf("the dropped answer is terminal — the run cannot recover what the runner had finished: %v", err)
	}
	// The retry keys on the cut, not on the prose: the same closed-pipe
	// text without the reaper behind it classifies as nothing, as before.
	if isDelegateRetryable(errors.New("claw backend: multiplexer: read |0: file already closed (stderr: )")) {
		t.Fatal("the plain closed-pipe text became retryable — the classifier is matching prose, not the cut")
	}
	// The cause stays reachable for callers that match on it.
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the closed-pipe cause is not reachable through errors.Is: %v", err)
	}
}

// Owning the pipe means closing the parent's write end at Start — the one
// half of StdoutPipe's choreography exec no longer does for the crossing.
// Until it closes, the launcher itself is a writer: a runner that exits
// without its result envelope and holds nothing cannot end the read at EOF,
// and the clean no-answer diagnostic degrades to the reaper's closed-read
// error.
func TestSandboxDispatch_ACleanDeathWithoutAnAnswerStillReadsEOF(t *testing.T) {
	dir := t.TempDir()
	b := newTestClawBackend()
	_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: &scriptedSandbox{script: awaitTaskEnvelope + "exit 0"},
	})
	if err == nil {
		t.Fatal("a runner that exited without its result envelope reported success")
	}
	if !strings.Contains(err.Error(), "exited without sending result envelope") {
		t.Fatalf("a clean death without an answer lost its EOF diagnostic: %v", err)
	}
}

// stdoutSettingSandbox is a driver whose Command captures the runner's
// stdout for itself — what StdoutPipe refused, and what the owned pipe must
// refuse just as loudly: assigning over it would silently take the pipe a
// driver captured for itself.
type stdoutSettingSandbox struct {
	*scriptedSandbox
}

func (s stdoutSettingSandbox) Driver() string { return "stdout-setting" }
func (s stdoutSettingSandbox) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	cmd := s.scriptedSandbox.Command(ctx, argv, opts)
	cmd.Stdout = io.Discard
	return cmd
}

// The refusal, and that it names the collision instead of clobbering.
func TestSandboxDispatch_RefusesADriverThatCapturesTheRunnerStdoutItself(t *testing.T) {
	dir := t.TempDir()
	b := newTestClawBackend()
	_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: stdoutSettingSandbox{scriptedSandbox: &scriptedSandbox{script: awaitTaskEnvelope + "exit 0"}},
	})
	if err == nil {
		t.Fatal("a driver that captured the runner's stdout was clobbered instead of refused")
	}
	if !strings.Contains(err.Error(), "already sets Stdout") {
		t.Fatalf("the refusal does not name the collision: %v", err)
	}
}

// hourBoundSandbox is a pipe-holding runner whose Cmd arrives with a bound of
// its OWN — an hour, far past anything a node waits for. A driver does set
// this field (pkg/sandbox/kubernetes: a credential plugin holding kubectl's
// pipes), so the launcher's assignment is an override, and the witness below
// is what makes it deliberate rather than silent.
type hourBoundSandbox struct{ inner *pipeHoldingSandbox }

func (s hourBoundSandbox) Driver() string { return "hour-bound" }
func (s hourBoundSandbox) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	cmd := s.inner.Command(ctx, argv, opts)
	cmd.WaitDelay = time.Hour
	return cmd
}
func (s hourBoundSandbox) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (s hourBoundSandbox) Cleanup(context.Context) error { return nil }

// The drain bound is the CROSSING's, not the driver's: a driver's WaitDelay
// bounds its own Wait, and nothing about it promises the engine gets this node
// back. A driver that chose an hour does not make a node wait an hour.
func TestSandboxDispatch_TheCrossingOwnsTheDrainBoundNotTheDriver(t *testing.T) {
	dir := t.TempDir()
	resultFile := resultEnvelopeFile(t, dir, delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	var logs strings.Builder
	b := loggedTestClawBackend(&logs)
	run := hourBoundSandbox{inner: newPipeHoldingSandbox(t, dir, awaitTaskEnvelope+"cat \"$3\"\nexit 0", resultFile)}
	done := make(chan error, 1)
	go func() {
		_, derr := b.executeViaSandboxRunner(context.Background(), delegate.Task{
			NodeID: "n", Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		})
		done <- derr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the node was failed by the abandoned drain: %v", err)
		}
		if !strings.Contains(logs.String(), "left its pipes held") {
			t.Errorf("the abandoned drain is unreported: %q", logs.String())
		}
	case <-time.After(sandboxDrainBound):
		t.Fatalf("the dispatch did not return in %s — the driver's own WaitDelay survived, so the crossing's bound is not the one that runs", sandboxDrainBound)
	}
}

// The cut's operation word follows the cause, not a fixed "read": the reaper
// closes BOTH owned ends, so the pipe operation that hits the close can be
// the multiplexer's read of stdout or its reply-write to stdin. os.File
// reports the operation through the *fs.PathError in the cause; a cause
// without one falls back to the operation-neutral word.
func TestErrDrainBoundCut_NamesTheOperationFromTheCause(t *testing.T) {
	read := &errDrainBoundCut{cause: &fs.PathError{Op: "read", Path: "|0", Err: os.ErrClosed}}
	if !strings.Contains(read.Error(), "the multiplexer's read ended") {
		t.Fatalf("a read cut is not named as a read: %v", read)
	}
	write := &errDrainBoundCut{cause: fmt.Errorf("delegate: write envelope: %w", &fs.PathError{Op: "write", Path: "|1", Err: os.ErrClosed})}
	if !strings.Contains(write.Error(), "the multiplexer's write ended") {
		t.Fatalf("a write cut is not named as a write: %v", write)
	}
	plain := &errDrainBoundCut{cause: fmt.Errorf("wrapped: %w", os.ErrClosed)}
	if !strings.Contains(plain.Error(), "the multiplexer's channel ended") {
		t.Fatalf("a cut whose cause carries no pipe error is not named operation-neutrally: %v", plain)
	}
}

// The drain bound can cut the multiplexer's REPLY-write, not only its read:
// after a tool_call or ask_user handler the loop writes the reply BEFORE it
// reads the next envelope, so a handler that outlasts the grace lands its
// reply on the reaper-closed stdin end — and the buffered result dies with
// the closed read end behind it. The typed cut names the operation that hit
// the close; the classification is the same transient the read shape gets,
// because the answer was lost either way and a re-run recovers it.
func TestSandboxDispatch_AReplyWritePastTheGraceIsNamedAsAWrite(t *testing.T) {
	dir := t.TempDir()
	callEnv, err := delegate.NewToolCallEnvelope("t1", "mcp.probe.slow", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resEnv, err := delegate.NewResultEnvelope(delegate.IOResult{Output: map[string]any{"verdict": "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	var says strings.Builder
	for _, env := range []delegate.Envelope{callEnv, resEnv} {
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		says.Write(raw)
		says.WriteByte('\n')
	}
	saysPath := filepath.Join(dir, "runner-says.ndjson")
	if err := os.WriteFile(saysPath, []byte(says.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// A full grace plus slack, timed from the envelope the launcher reads
	// first; derived from the constant, so retuning the bound does not make
	// this vacuous.
	slower := clawPipeDrainGrace + time.Second
	toolRan := false
	slowTool := delegate.ToolDef{
		Name: "mcp.probe.slow",
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			time.Sleep(slower)
			toolRan = true
			return "done", nil
		},
	}
	b := newTestClawBackend()
	run := &scriptedSandbox{
		script: awaitTaskEnvelope + `head -n 1 "$1"` + "\nsleep 0.2\n" + `tail -n 1 "$1"` + "\nexit 0",
		args:   []string{saysPath},
	}
	_, err = b.executeViaSandboxRunner(context.Background(), delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir, Sandbox: run,
		ToolDefs: []delegate.ToolDef{slowTool},
	})
	if !toolRan {
		t.Fatal("the slow tool never ran — this witness no longer drives the shape it guards")
	}
	if err == nil {
		t.Fatal("the dispatch succeeded past the grace — the drain bound no longer cuts, and this witness no longer drives the trade")
	}
	// The same transient the read shape gets: the runner exited 0 and its
	// answer was lost either way.
	if !isDelegateRetryable(err) {
		t.Fatalf("the reply-write cut is terminal — a re-run recovers what the runner had finished: %v", err)
	}
	var cut *errDrainBoundCut
	if !errors.As(err, &cut) {
		t.Fatalf("the reply-write failure lost the typed cut: %v", err)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the closed-stdin cause is not reachable through errors.Is: %v", err)
	}
	// The operation named is the one that hit the close: the reply is a
	// write, and a fixed "read" would misdirect the operator to the runner's
	// stdout where nothing was ever read past the handler.
	if !strings.Contains(err.Error(), "the multiplexer's write ended") {
		t.Fatalf("the reply-write cut is not named as a write: %v", err)
	}
	if strings.Contains(err.Error(), "the multiplexer's read ended") {
		t.Fatalf("the write cut is named as a read — the operation is asserted, not derived from the cause: %v", err)
	}
}

// Ownership starts BEFORE the pipes exist: both driver refusals run with no
// pipe created, so a refused driver leaves nothing behind, and the only
// error path that can run once the stdin pair exists (the stdout os.Pipe
// failure) releases the pair it would strand. The collector is off so the
// finalizers cannot pay for ends a refused dispatch leaves behind, and the
// final delta is sign-aware: a strand can only ADD descriptors, so only a
// positive delta convicts — a negative one is a foreign close (the
// finalizer goroutine drains its queue of descriptors an earlier test left
// un-finalized even with the collector off), and the count pair is
// re-placed, not believed.
func TestSandboxDispatch_TheStdoutRefusalLeavesNoPipeBehind(t *testing.T) {
	dir := t.TempDir()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const attempts = 200
	for try := 0; ; try++ {
		refsBefore := countSelfFDs(t)
		for i := 0; i < attempts; i++ {
			b := newTestClawBackend()
			_, err := b.executeViaSandboxRunner(context.Background(), delegate.Task{
				NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
				Sandbox: stdoutSettingSandbox{scriptedSandbox: &scriptedSandbox{script: awaitTaskEnvelope + "exit 0"}},
			})
			if err == nil || !strings.Contains(err.Error(), "already sets Stdout") {
				t.Fatalf("attempt %d: expected the Stdout refusal, got %v", i, err)
			}
		}
		if got := countSelfFDs(t) - refsBefore; got > 0 {
			t.Fatalf("a refused driver left %d fds behind over %d refusals — the refusal ran with pipes already owned", got, attempts)
		} else if got < 0 {
			if try >= 4 {
				t.Fatalf("the descriptor table kept shrinking across 5 count pairs (last delta: %d)", got)
			}
			continue
		}
		return
	}
}

// countSelfFDs counts this process's open descriptors and makes the set
// dense: every number up to the highest open one that is free is filled (the
// listing's own directory handle excluded, and the hole its close leaves
// covered), so a caller can place an RLIMIT_NOFILE whose next free numbers
// are exactly the ones the next opens will take.
func countSelfFDs(t *testing.T) int {
	t.Helper()
	listFDs := func() map[int]bool {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skipf("no /proc/self/fd: %v", err)
		}
		open := map[int]bool{}
		for _, e := range entries {
			if _, err := strconv.Atoi(e.Name()); err != nil {
				continue
			}
			if target, err := filepath.EvalSymlinks("/proc/self/fd/" + e.Name()); err == nil && target == "/proc/self/fd" {
				continue // the listing's own directory handle
			}
			n, _ := strconv.Atoi(e.Name())
			open[n] = true
		}
		return open
	}
	open := listFDs()
	highest := 0
	for n := range open {
		if n > highest {
			highest = n
		}
	}
	var fillers []*os.File
	t.Cleanup(func() {
		for _, f := range fillers {
			_ = f.Close()
		}
	})
	for n := 0; n <= highest; n++ {
		if open[n] {
			continue
		}
		f, err := os.Open("/dev/null")
		if err != nil {
			t.Fatalf("fill the descriptor hole at %d: %v", n, err)
		}
		fillers = append(fillers, f)
	}
	// The listing's handle closed after the first read and the fills took
	// the holes, so the set is now dense 0..highest — prove it, or the
	// rlimit a caller places on top of this count sits on a guess.
	for n, isOpen := range listFDs() {
		if n <= highest && !isOpen {
			t.Fatalf("the descriptor set is not dense after filling: %d is free", n)
		}
	}
	return highest + 1
}

// The stdout os.Pipe failure is the one error path that can strand the stdin
// pair — and it fires exactly when descriptor numbers are scarce, the moment
// a stranded pair costs the most. The cap is placed by retries until the
// shape lands: descriptors this test binary still holds un-finalized from
// earlier tests can close under the finalizer goroutine at any moment — its
// queue drains whether or not the collector is off — so a single placement
// reads as a guess; the retry re-counts a drained, collector-off table and
// places the cap two numbers past its top, where the dispatch's stdin pair
// takes the last two slots and the stdout pipe finds none. The dispatch must
// then fail at the stdout pipe AND leave the count where it was — read with
// the collector still off, so no cycle here can finalize the very pair the
// check hunts — and only a positive delta convicts: a strand can only add
// descriptors, a foreign close shrinks the table, and the placement is
// redone, not believed.
func TestSandboxDispatch_TheStdoutPipeFailureReleasesTheStdinPair(t *testing.T) {
	dir := t.TempDir()
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Skipf("no RLIMIT_NOFILE: %v", err)
	}
	saved := lim.Cur
	t.Cleanup(func() {
		lim.Cur = saved
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	})
	b := newTestClawBackend()
	task := delegate.Task{
		NodeID: "n", Iteration: 1, Model: "openai/gpt-5", WorkDir: dir,
		Sandbox: &scriptedSandbox{script: awaitTaskEnvelope + "exit 0"},
	}
	for attempt := 0; ; attempt++ {
		// Drain the finalizable garbage earlier tests left, then freeze the
		// table: a count or a placement over a table the collector is still
		// shrinking sits on moving ground.
		runtime.GC()
		runtime.GC()
		gcOff := debug.SetGCPercent(-1)
		top := countSelfFDs(t)
		placed := false
		if lim.Max >= uint64(top+2) {
			lim.Cur = uint64(top + 2)
			placed = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim) == nil
		}
		if !placed {
			debug.SetGCPercent(gcOff)
			t.Skipf("cannot place the descriptor cap at %d (hard cap %d)", top+2, lim.Max)
		}
		_, err := b.executeViaSandboxRunner(context.Background(), task)
		lim.Cur = saved
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
		if err == nil || !strings.Contains(err.Error(), "stdout pipe") {
			debug.SetGCPercent(gcOff)
			if attempt >= 4 {
				t.Fatalf("the dispatch did not fail at the stdout pipe in 5 placements (last: %v)", err)
			}
			continue
		}
		// The strand can only ADD descriptors, so only a positive delta
		// convicts, and the count is read with the collector still off: a
		// GC cycle here could finalize the very pair the check hunts. A
		// negative delta is a foreign close — the finalizer goroutine
		// drains its queue of descriptors an earlier test left
		// un-finalized even with the collector off — and the placement is
		// redone, not believed.
		got := countSelfFDs(t) - top
		debug.SetGCPercent(gcOff)
		if got > 0 {
			t.Fatalf("the stdout-pipe failure stranded %d fds — the stdin pair it left behind was not released", got)
		}
		if got < 0 {
			if attempt >= 4 {
				t.Fatalf("the descriptor table kept drifting across 5 placements (last delta: %d)", got)
			}
			continue
		}
		return
	}
}
