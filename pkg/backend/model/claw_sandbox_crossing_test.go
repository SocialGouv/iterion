package model

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
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
// recovers that node is the run's cancellation, which closes this process's
// pipe ends clawRunnerWaitDelay later, and the dispatch then returns. This
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
