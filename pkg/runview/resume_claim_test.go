package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	_, hash, err := CompileWorkflowWithHash(botPath)
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

// TestResumeClaim_anAdmissionWinsOverAnEndSeenWithIt: a resume past its
// refusals that then failed at once started — its failure is the run's
// outcome, never reported as a refusal, whichever of the two its caller sees
// first. An end before the admission is the caller's error.
func TestResumeClaim_anAdmissionWinsOverAnEndSeenWithIt(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := newResumeClaim()
		c.admit()
		c.end(errors.New("the first node failed"))
		res := &LaunchResult{RunID: "run"}
		if got, err := c.await(context.Background(), res); got != res || err != nil {
			t.Fatalf("a resume past its refusals, then failed: got %v, %v, want it started", got, err)
		}
	}
	c := newResumeClaim()
	c.end(errors.New("refused before the claim"))
	if got, err := c.await(context.Background(), &LaunchResult{RunID: "run"}); got != nil || err == nil {
		t.Fatalf("a resume that ended before its admission: got %v, %v, want its error", got, err)
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
	_, hash, err := CompileWorkflowWithHash(botPath)
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

// holdingBot is a bundle whose one node holds the workspace for 4 seconds.
const holdingBot = `
schema o:
  ok: bool

tool hold:
  command: ` + "`sleep 4; printf '{\"ok\":true}'`" + `
  output: o

workflow w:
  worktree: none
  repo_devbox: off
  entry: hold
  hold -> done
`

// TestResume_doesNotWaitOnAnotherRunsNode: an in-process resume returns once
// its engine is past its refusals, not at its claim — the claim follows the
// workspace's resources, which wait out every node another run executes in
// the same directory, and nothing makes its caller wait for that run.
func TestResume_doesNotWaitOnAnotherRunsNode(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bdir := filepath.Join(root, "bot")
	for _, d := range []string{work, filepath.Join(bdir, "skills")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mainBot := filepath.Join(bdir, "main.bot")
	if err := os.WriteFile(mainBot, []byte(holdingBot), 0o644); err != nil {
		t.Fatal(err)
	}
	_, cs, b, err := compileForLaunch(mainBot, "", "")
	if err != nil || b == nil {
		t.Fatalf("compile: bundle=%v err=%v", b != nil, err)
	}
	svc, err := NewService(root, WithLogger(iterlog.Nop()), WithWorkDir(work))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const paused = "run-paused-beside"
	if _, err := svc.store.CreateRun(ctx, paused, "w", nil); err != nil {
		t.Fatal(err)
	}
	r, err := svc.store.LoadRun(ctx, paused)
	if err != nil {
		t.Fatal(err)
	}
	r.WorkflowHash, r.WorkDir, r.FilePath, r.BundlePath = cs.Hash, work, mainBot, bdir
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SaveCheckpoint(ctx, paused, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpdateRunStatus(ctx, paused, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatal(err)
	}
	holding, err := svc.Launch(ctx, LaunchSpec{FilePath: mainBot, WorkDir: work})
	if err != nil {
		t.Fatalf("launch the holding run: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for started := false; !started; {
		evs, _ := svc.store.LoadEvents(ctx, holding.RunID)
		for _, ev := range evs {
			started = started || (ev.Type == store.EventNodeStarted && ev.NodeID == "hold")
		}
		if !started && time.Now().After(deadline) {
			t.Fatal("the holding run never started its node")
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	res, err := svc.Resume(ctx, ResumeSpec{RunID: paused, FilePath: mainBot})
	elapsed := time.Since(start)
	if res != nil {
		<-res.Done
	}
	<-holding.Done
	if err != nil || elapsed > 2*time.Second {
		t.Fatalf("Service.Resume beside another run's node: err=%v after %s, want it started at once", err, elapsed.Truncate(time.Millisecond))
	}
}
