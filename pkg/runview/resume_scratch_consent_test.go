package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// consentCapturePublisher records the resume spec it is handed.
type consentCapturePublisher struct {
	operatorResumePublisher
	spec ResumeSpec
}

func (p *consentCapturePublisher) SubmitResume(_ context.Context, spec ResumeSpec, _ *ir.Workflow, _ *CompiledSource) error {
	p.resumeCalls++
	p.spec = spec
	return nil
}

// TestResume_aScratchLossIsShownBeforeAForceableRefusal: a run whose bank it
// has moved past — an execution restored it, ran a sandbox node, and lost its
// pod without a teardown, so its latest execution wrote no record of its own
// — resumed on an edited source. About to refuse the source change, which
// --force accepts, the surface shows the stale bank first, whose loss --force
// does not accept, naming both: the operator sees every consent the resume
// needs at once, and nothing is published. Given both, it is published with
// the scratch's consent.
func TestResume_aScratchLossIsShownBeforeAForceableRefusal(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	if err := os.WriteFile(botPath, []byte("\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	publisher := &consentCapturePublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(publisher))
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-stale-before-force"
	_, hash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatal(err)
	}
	seedPausedOperatorRun(t, svc, runID, hash)
	ctx := context.Background()
	for _, ev := range []store.Event{
		{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": true, "bytes": 42}},
		{Type: store.EventRunResumed},
		{Type: store.EventSandboxScratchRestored, Data: map[string]any{"restored": true, "bytes": 42}},
		{Type: store.EventNodeFinished, NodeID: "work", Data: map[string]any{"_in_sandbox": true, "_on_cycle": false}},
	} {
		if _, err := svc.store.AppendEvent(ctx, runID, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(botPath, []byte("\n## edited\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pfErr := svc.PreflightResume(ctx, ResumeSpec{RunID: runID, FilePath: botPath})
	_, rErr := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath})
	for surface, err := range map[string]error{"PreflightResume": pfErr, "Resume": rErr} {
		var rt *runtime.RuntimeError
		if !errors.As(err, &rt) || rt.Code != runtime.ErrCodeScratchNotPortable || !rt.AlsoNeedsForce || runtime.IsWorkflowSourceChanged(err) || !namesSourceChange(err) {
			t.Fatalf("%s, unforced, over a stale bank and an edited source: %v, want the stale bank shown first, naming the source change and both consents", surface, err)
		}
	}
	if publisher.resumeCalls != 0 {
		t.Fatalf("the refused resumes published %d", publisher.resumeCalls)
	}
	if _, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath, Force: true, AcceptScratchLoss: true}); err != nil || publisher.resumeCalls != 1 || !publisher.spec.AcceptScratchLoss {
		t.Fatalf("the resume given both consents: %v, published %d, consent carried %v", err, publisher.resumeCalls, publisher.spec.AcceptScratchLoss)
	}
}
