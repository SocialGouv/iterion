package runview

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedStaleBankPark leaves runID's bank stale behind an execution that wrote
// no record of its own: the bank, a resume that restored it, then a node of
// that execution that ran in its sandbox — a park the surface leaves to the
// engine, whose check refuses it before its claim.
func seedStaleBankPark(t *testing.T, st store.RunStore, runID string) {
	t.Helper()
	for _, ev := range []store.Event{
		{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": true, "bytes": 42}},
		{Type: store.EventRunResumed},
		{Type: store.EventSandboxScratchRestored, Data: map[string]any{"restored": true, "bytes": 42}},
		{Type: store.EventNodeFinished, NodeID: "work", Data: map[string]any{"_in_sandbox": true, "_on_cycle": false}},
	} {
		if _, err := st.AppendEvent(context.Background(), runID, ev); err != nil {
			t.Fatal(err)
		}
	}
}

// TestResume_aRefusalBeforeTheClaimIsTheCallersError: an in-process resume
// the engine refuses before its claim — a stale bank the surface left to it —
// is the caller's error, as the surface's own refusals are: the run did not
// move, and a resume reported started would say it had. Accepting the
// scratch's loss, the same resume starts and runs.
func TestResume_aRefusalBeforeTheClaimIsTheCallersError(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	if err := os.WriteFile(botPath, []byte("\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := CompileWorkflowWithHash(botPath, "")
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-inproc-stale"
	seedPausedOperatorRun(t, svc, runID, hash)
	seedStaleBankPark(t, svc.store, runID)
	ctx := context.Background()

	res, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath})
	if res != nil || !scratchRefused(err) {
		t.Fatalf("an in-process resume the engine refuses before its claim: got res=%v err=%v, want SCRATCH_NOT_PORTABLE and no started resume", res != nil, err)
	}
	if r, err := svc.store.LoadRun(ctx, runID); err != nil || r.Status != store.RunStatusPausedOperator {
		t.Fatalf("the refused resume moved the run: %v (%v)", r.Status, err)
	}

	res, err = svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath, AcceptScratchLoss: true})
	if err != nil || res == nil {
		t.Fatalf("the resume accepting the scratch's loss: %v, want it started", err)
	}
	select {
	case <-res.Done:
	case <-time.After(20 * time.Second):
		t.Fatal("the accepted resume never ended")
	}
	if r, err := svc.store.LoadRun(ctx, runID); err != nil || r.Status != store.RunStatusFinished {
		t.Fatalf("the accepted resume left the run %v (%v), want finished", r.Status, err)
	}
}

// TestResumeClaim_theClaimDecides: a resume that claimed the run, then
// failed at once, started — its failure is the run's outcome, whichever of
// the two its caller sees first. One that ended without a claim is refused,
// past the engine's own refusals or not: its path refused it. One past its
// refusals that neither claims nor ends within the grace (its claim waits for
// another run's node) is answered started.
func TestResumeClaim_theClaimDecides(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := newResumeClaim()
		c.admit()
		c.claim()
		c.end(errors.New("the first node failed"))
		res := &LaunchResult{RunID: "run"}
		if got, err := c.await(context.Background(), res); got != res || err != nil {
			t.Fatalf("a resume that claimed the run, then failed: got %v, %v, want it started", got, err)
		}
	}
	for _, admitted := range []bool{false, true} {
		c := newResumeClaim()
		if admitted {
			c.admit()
		}
		c.end(errors.New("refused before the claim"))
		if got, err := c.await(context.Background(), &LaunchResult{RunID: "run"}); got != nil || err == nil {
			t.Fatalf("a resume that ended before any claim (admitted=%v): got %v, %v, want its error", admitted, got, err)
		}
	}
	c := newResumeClaim()
	c.admit()
	start := time.Now()
	res := &LaunchResult{RunID: "run"}
	if got, err := c.await(context.Background(), res); got != res || err != nil || time.Since(start) < resumeRefusalGrace {
		t.Fatalf("a resume past its refusals whose claim waits: got %v, %v after %s, want it started once the grace ran out", got, err, time.Since(start))
	}
}

// TestResume_aClaimedResumeReturnsAtItsClaim: an in-process resume returns
// once its engine claimed the run, not when the run ends — here the run
// pauses again at its human gate, which is its outcome, not the resume's
// error.
func TestResume_aClaimedResumeReturnsAtItsClaim(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	dir := t.TempDir()
	botPath := filepath.Join(dir, "gate.bot")
	src := pausingUnitGate + "\nworkflow gated:\n  entry: gate\n  gate -> done\n"
	if err := os.WriteFile(botPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := CompileWorkflowWithHash(botPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const runID = "run-inproc-gate"
	if _, err := svc.store.CreateRun(ctx, runID, "gated", nil); err != nil {
		t.Fatal(err)
	}
	r, err := svc.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkflowHash = hash
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpdateRunStatus(ctx, runID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath})
	if err != nil || res == nil {
		t.Fatalf("a resume its engine claims: %v, want it started", err)
	}
	select {
	case <-res.Done:
	case <-time.After(20 * time.Second):
		t.Fatal("the resumed run never paused")
	}
	if r, err := svc.store.LoadRun(ctx, runID); err != nil || r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("the resumed run is %v (%v), want paused at its gate", r.Status, err)
	}
}

const (
	// holdBound bounds the hold in WALL CLOCK, which is what orders it
	// against the reaper that has to outlive it. A hold bounded by a count of
	// polls takes longer the more loaded the machine is — 1200 steps of 50ms
	// measured at 61.6s idle and 83.5s under twenty burners on two cores — so
	// no fixed reaper patience is above it. Bounded by the clock, load only
	// makes the polling coarser.
	holdBound = 60 * time.Second
	// holdReaperMargin is what holdTheWorkspace's reaper adds to holdBound so
	// it gives up strictly AFTER the node it waits for, never before it.
	holdReaperMargin = 30 * time.Second
	holdPollInterval = 50 * time.Millisecond
)

// holdScript is the shell the holding node runs: it waits for `release` to
// appear, up to bound, and exits NON-ZERO when it never did — a test that dies
// without releasing leaves a node that fails loudly, not one that spins for
// the suite's life.
//
// The clock is bash's own $SECONDS, so the wait costs no fork per poll and
// stays a wall-clock bound under any load. Its granularity is one second,
// which is also the shortest bound this can express.
func holdScript(release string, bound time.Duration) string {
	seconds := int(bound.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return `r='` + release + `'; while [ ! -e "$r" ]; do [ "$SECONDS" -ge ` + strconv.Itoa(seconds) + ` ] && break; ` +
		`sleep ` + strconv.FormatFloat(holdPollInterval.Seconds(), 'f', -1, 64) + `; done; ` +
		`[ -e "$r" ] || { echo "hold: the release file $r never appeared within ` + strconv.Itoa(seconds) + `s" >&2; exit 1; }; ` +
		`printf '{"ok":true}'`
}

// holdingBot is a bundle whose one node holds the workspace until `release`
// exists, and ends on nothing else. The hold's length belongs to the test,
// which is what lets a test witness something DURING it rather than race a
// sleep.
func holdingBot(release string) string {
	hold := holdScript(release, holdBound)
	return `
schema o:
  ok: bool

tool hold:
  command: ` + "`" + hold + "`" + `
  output: o

workflow w:
  worktree: none
  repo_devbox: off
  entry: hold
  hold -> done
`
}

// heldWorkspace is the layout a test beside a holding run needs: a service
// root, the work directory two runs share, and the bundle whose node holds it.
type heldWorkspace struct {
	root, work, bundleDir, botPath, release string
}

// aWorkspaceHeldByAnotherRun builds that layout, the Service rooted on it and
// the run whose node holds the work directory, and comes back once that node
// is executing — so the workspace is genuinely held when its caller acts.
//
// One entry point, on purpose: the hold is only safe with the reaper
// holdTheWorkspace arms, and a workspace handed back before its hold is
// launched is a node that waits out holdBound on a release file the framework
// removed along with its directory. It outlives the suite, and internal/
// proctest then reports a surviving `bash` instead of the assertion that
// actually failed.
func aWorkspaceHeldByAnotherRun(t *testing.T, ctx context.Context, logger *iterlog.Logger) (heldWorkspace, *Service, *LaunchResult) {
	t.Helper()
	ws := heldWorkspace{root: t.TempDir(), release: filepath.Join(t.TempDir(), "release-the-hold")}
	ws.work, ws.bundleDir = filepath.Join(ws.root, "work"), filepath.Join(ws.root, "bot")
	for _, d := range []string{ws.work, filepath.Join(ws.bundleDir, "skills")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws.botPath = filepath.Join(ws.bundleDir, "main.bot")
	if err := os.WriteFile(ws.botPath, []byte(holdingBot(ws.release)), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(ws.root, WithLogger(logger), WithWorkDir(ws.work))
	if err != nil {
		t.Fatal(err)
	}
	return ws, svc, holdTheWorkspace(t, ctx, svc, ws)
}

// holdTheWorkspace launches the run whose node holds ws.work and returns once
// that node is executing, so the workspace is genuinely held when its caller
// acts.
//
// It arms the release as a cleanup that waits for the run to END, and the wait
// is the load-bearing half: the release file lives under a t.TempDir() that a
// cleanup queued behind this one removes, so a cleanup that writes it and
// returns gives the file a life shorter than the hold's own poll interval. The
// node then spins out its whole holdBound on a file that is no longer there,
// outliving the suite — where internal/proctest reports a surviving `bash`
// instead of the assertion that actually failed. The patience is holdBound
// plus a margin, so the reaper outlasts the node it reaps.
func holdTheWorkspace(t *testing.T, ctx context.Context, svc *Service, ws heldWorkspace) *LaunchResult {
	t.Helper()
	holding, err := svc.Launch(ctx, LaunchSpec{FilePath: ws.botPath, WorkDir: ws.work})
	if err != nil {
		t.Fatalf("launch the holding run: %v", err)
	}
	t.Cleanup(func() {
		ws.releaseTheHold(t)
		select {
		case <-holding.Done:
		case <-time.After(holdBound + holdReaperMargin):
			// Errorf, not Fatalf: a cleanup that calls FailNow abandons
			// the cleanups queued behind it.
			t.Errorf("the holding run never ended after its release (%s)", ws.release)
		}
	})
	awaitHoldingNode(t, svc, holding.RunID)
	return holding
}

// releaseTheHold ends the holding run's node. Idempotent: a test releases
// where its witness is taken, and the cleanup releases again.
func (ws heldWorkspace) releaseTheHold(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(ws.release, nil, 0o644); err != nil {
		t.Errorf("release the holding run's node (%s): %v", ws.release, err)
	}
}

// awaitHoldingNode waits until runID's hold node has started, so the workspace
// is genuinely held when the test acts.
func awaitHoldingNode(t *testing.T, svc *Service, runID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if holdNodeEvent(t, svc, runID, store.EventNodeStarted) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the holding run never started its node")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// holdNodeEvent reports whether runID's hold node carries the given event.
func holdNodeEvent(t *testing.T, svc *Service, runID string, typ store.EventType) bool {
	t.Helper()
	evs, err := svc.store.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("the holding run's events: %v", err)
	}
	for _, ev := range evs {
		if ev.Type == typ && ev.NodeID == "hold" {
			return true
		}
	}
	return false
}

// awaitRunDone waits for a run's Done, and fails the test rather than hanging
// when it never comes.
func awaitRunDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("%s never ended", what)
	}
}

// resumeBeside calls Resume beside a holding run and fails the test rather
// than hanging when the call does not come back. The hold ends on a file the
// test has not written yet, so a resume that waits for the other run's node
// waits for ever: an unbounded call here would turn that regression into a
// suite that never finishes. Both runs are let go by holdTheWorkspace's
// cleanup, which the failure below reaches like any other exit.
func resumeBeside(t *testing.T, ctx context.Context, svc *Service, spec ResumeSpec) (*LaunchResult, error) {
	t.Helper()
	type resumed struct {
		res *LaunchResult
		err error
	}
	returned := make(chan resumed, 1)
	go func() {
		res, err := svc.Resume(ctx, spec)
		returned <- resumed{res, err}
	}()
	select {
	case got := <-returned:
		return got.res, got.err
	case <-time.After(30 * time.Second):
		t.Fatalf("Service.Resume of run %q never returned beside another run's node: it waited for that run's node", spec.RunID)
		return nil, nil
	}
}

// TestTheHoldFailsLoudlyWhenItsReleaseNeverComes: the holding node's wait ends
// on its own, non-zero, within a bound the CLOCK decides. That is what orders
// it against holdTheWorkspace's reaper, which waits holdBound +
// holdReaperMargin for exactly this node: a wait whose length depends on the
// machine's load outlasts any fixed patience, and the reaper then abandons
// first, leaving the queued t.TempDir removals to race a live `bash`.
func TestTheHoldFailsLoudlyWhenItsReleaseNeverComes(t *testing.T) {
	const bound = time.Second
	// Generous: the point is that the hold ends by ITSELF, so a context that
	// had to end it is the failure, not the measurement.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	never := filepath.Join(t.TempDir(), "release-that-never-comes")
	start := time.Now()
	out, err := exec.CommandContext(ctx, "bash", "-c", holdScript(never, bound)).CombinedOutput()
	waited := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("the hold was still waiting after %v for a %v bound: its wait is not bounded by the clock, so the reaper that gives it holdBound+%v gives up first:\n%s", waited, bound, holdReaperMargin, out)
	}
	if err == nil {
		t.Fatalf("the hold reported success with its release file absent: a leaked node would finish its run quietly instead of saying what it waited for:\n%s", out)
	}
	if !strings.Contains(string(out), never) {
		t.Fatalf("the hold does not name the release it waited for, so a leak reads as an unexplained node failure:\n%s", out)
	}
	if waited > bound+10*time.Second {
		t.Fatalf("the hold took %v to honour a %v bound: a bound that stretches with the machine's load is not above the reaper's patience", waited, bound)
	}
}

// TestResume_doesNotWaitOnAnotherRunsNode: an in-process resume returns once
// its engine is past its refusals, not at its claim — the claim follows the
// workspace's resources, which wait out every node another run executes in
// the same directory, and nothing makes its caller wait for that run.
//
// The witness is a happens-before, not a clock: the other run's node is held
// by a file only this test writes, so a Resume that returns BEFORE that write
// cannot have waited for it. A Resume that does wait never returns here and
// is failed, loudly, by the bound below (#2161).
//
// How long the caller may sit before it is answered started is the second half
// of the same property, and it has two readers below, because either one alone
// is blind to the other's regression. The CONSTANT bounds what the grace is
// allowed to BE — a clock cannot pin one second without flaking on a loaded
// runner, and nothing else in the tree bounds it. The OBSERVED wait bounds
// what the caller's path DOES with that constant: a second wait, or arithmetic
// on the way to the timer, moves the caller by ten times the grace while the
// constant stays exactly where it is.
func TestResume_doesNotWaitOnAnotherRunsNode(t *testing.T) {
	if resumeRefusalGrace > 2*time.Second {
		t.Fatalf("an in-process resume now sits %v behind another run's node before it is answered started: "+
			"the studio's answer-human round trip and `iterion resume` wait that long for nothing", resumeRefusalGrace)
	}
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	ctx := context.Background()
	ws, svc, holding := aWorkspaceHeldByAnotherRun(t, ctx, iterlog.Nop())
	_, cs, b, err := compileForLaunch(ws.botPath, "", "", "")
	if err != nil || b == nil {
		t.Fatalf("compile: bundle=%v err=%v", b != nil, err)
	}
	const paused = "run-paused-beside"
	if _, err := svc.store.CreateRun(ctx, paused, "w", nil); err != nil {
		t.Fatal(err)
	}
	r, err := svc.store.LoadRun(ctx, paused)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkflowHash, r.WorkDir, r.FilePath, r.BundlePath = cs.Hash, ws.work, ws.botPath, ws.bundleDir
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SaveCheckpoint(ctx, paused, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpdateRunStatus(ctx, paused, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := resumeBeside(t, ctx, svc, ResumeSpec{RunID: paused, FilePath: ws.botPath})
	waited := time.Since(start)
	if err != nil || res == nil {
		t.Fatalf("Service.Resume beside another run's node: res=%v err=%v, want it started", res != nil, err)
	}
	// That grace is the only wait this call may make. The bound is ten times
	// it: the longest legitimate wait on record is 2.782s — the grace plus
	// 1.78s of scheduling on a loaded full-suite run — so a starved runner
	// keeps 3.6× that, and the call itself measures 1.02-1.15s under twenty
	// burners on two cores. Unlike reading the constant, this catches
	// arithmetic on the way to the timer that moves the caller to ten times
	// the grace or past it (a timer never fires early, so a ten-fold one
	// lands beyond the bound by the call's own work). A caller that never
	// comes back is resumeBeside's 30s fatal, not this.
	if waited > 10*resumeRefusalGrace {
		t.Fatalf("Service.Resume sat %v behind another run's node for a %v grace: its caller waits on something besides that grace",
			waited.Truncate(time.Millisecond), resumeRefusalGrace)
	}
	// The hold ends on a file this test has not written yet, so this is the
	// whole property: the resume came back while the other run's node was
	// still executing. A hold that ended early would make the return say
	// nothing, and that is a broken fixture, not a passing test.
	if holdNodeEvent(t, svc, holding.RunID, store.EventNodeFinished) {
		t.Fatal("the holding run's node had already ended when Resume returned: the hold was not holding, and this run witnesses nothing")
	}

	ws.releaseTheHold(t)
	awaitRunDone(t, res.Done, "the resumed run")
	awaitRunDone(t, holding.Done, "the holding run")
	// The hold ran its wait through and printed its output: the node that was
	// still executing above was executing THAT. A node that had died on its
	// own emits no node_finished either, and would make the witness vacuous.
	held, err := svc.store.LoadRun(ctx, holding.RunID)
	if err != nil {
		t.Fatalf("the holding run's record: %v", err)
	}
	if held.Status != store.RunStatusFinished {
		t.Fatalf("the holding run is %v, want finished — its node did not run to the end the wait this test leans on", held.Status)
	}
}

// TestResume_aPathsRefusalIsTheCallersError: a refusal the resume's path
// makes after the engine's own — an await gate with a question still
// unanswered — is the in-process caller's error too, and the run stays paused.
func TestResume_aPathsRefusalIsTheCallersError(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	if err := os.WriteFile(botPath, []byte("\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := CompileWorkflowWithHash(botPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const runID = "run-await-one-unanswered"
	if _, err := svc.store.CreateRun(ctx, runID, "operator_resume", nil); err != nil {
		t.Fatal(err)
	}
	r, err := svc.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkflowHash = hash
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q1", "q2"} {
		if err := svc.store.WriteInteraction(ctx, &store.Interaction{ID: id, RunID: runID, NodeID: "asker", Kind: store.InteractionKindAsync, RequestedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.store.PauseRun(ctx, runID, &store.Checkpoint{
		NodeID:        "asker",
		InteractionID: "pause-1",
		InteractionQuestions: map[string]any{
			delegate.AwaitPendingInteractionsKey: delegate.AwaitPendingToQuestions([]delegate.PendingAsync{{InteractionID: "q1"}, {InteractionID: "q2"}}),
		},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath, Answers: map[string]any{"q1": "blue"}})
	if res != nil || err == nil || !strings.Contains(err.Error(), "still unanswered") {
		t.Fatalf("a resume its path refuses (q2 unanswered): started=%v err=%v, want the refusal", res != nil, err)
	}
	if got, err := svc.store.LoadRun(ctx, runID); err != nil || got.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("the refused resume moved the run: %v (%v)", got.Status, err)
	}
}

// TestResume_aLateRefusalIsSaidInTheRunsLog: a path's refusal that comes after
// the grace — the engine first waited for the workspace, held by another run's
// node — reaches no caller: it answered "started". It is said in the run's
// log, where the run is read.
func TestResume_aLateRefusalIsSaidInTheRunsLog(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	ctx := context.Background()
	ws, svc, holding := aWorkspaceHeldByAnotherRun(t, ctx, iterlog.New(iterlog.LevelInfo, io.Discard))
	_, cs, _, err := compileForLaunch(ws.botPath, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const paused = "run-await-beside"
	if _, err := svc.store.CreateRun(ctx, paused, "w", nil); err != nil {
		t.Fatal(err)
	}
	r, err := svc.store.LoadRun(ctx, paused)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkflowHash, r.WorkDir, r.FilePath, r.BundlePath = cs.Hash, ws.work, ws.botPath, ws.bundleDir
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q1", "q2"} {
		if err := svc.store.WriteInteraction(ctx, &store.Interaction{ID: id, RunID: paused, NodeID: "asker", Kind: store.InteractionKindAsync, RequestedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.store.PauseRun(ctx, paused, &store.Checkpoint{
		NodeID:        "asker",
		InteractionID: "pause-1",
		InteractionQuestions: map[string]any{
			delegate.AwaitPendingInteractionsKey: delegate.AwaitPendingToQuestions([]delegate.PendingAsync{{InteractionID: "q1"}, {InteractionID: "q2"}}),
		},
	}); err != nil {
		t.Fatal(err)
	}
	res, err := resumeBeside(t, ctx, svc, ResumeSpec{RunID: paused, FilePath: ws.botPath, Answers: map[string]any{"q1": "blue"}})
	if err != nil || res == nil {
		t.Fatalf("a resume waiting on another run's node: err=%v, want it answered started after the grace", err)
	}
	// Answered BEFORE the hold ends — the refusal below is what the resume's
	// path says afterwards, with nobody left to tell.
	if holdNodeEvent(t, svc, holding.RunID, store.EventNodeFinished) {
		t.Fatal("the holding run's node had already ended when Resume returned: the refusal this test reads was not a late one")
	}
	ws.releaseTheHold(t)
	awaitRunDone(t, res.Done, "the resumed run")
	awaitRunDone(t, holding.Done, "the holding run")
	log, err := os.ReadFile(filepath.Join(ws.root, "runs", paused, "run.log"))
	if err != nil {
		t.Fatalf("the run's log: %v", err)
	}
	if !strings.Contains(string(log), "refused before it claimed the run") || !strings.Contains(string(log), "still unanswered") {
		t.Fatalf("the late refusal is not in the run's log:\n%s", log)
	}
	if got, err := svc.store.LoadRun(ctx, paused); err != nil || got.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("the refused resume moved the run: %v (%v)", got.Status, err)
	}
}
