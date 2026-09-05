package cloudpublisher

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A machine-initiated resume (Automatic) of a cancelled run must be refused
// at the publisher boundary too: the run doc stays cancelled with its cancel
// reason intact, and nothing reaches the queue. An operator's resume of the
// same doc still proceeds — cancelled is theirs to override.
func TestSubmitResume_AutomaticRefusesCancelled(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-cancelled-by-stop-on-close"
	cancelReason := store.RunEndReasonPRClosed + " (was failed_resumable: node \"campaign\": rate_limited)"
	if err := st.SaveRun(ctx, &store.Run{
		ID: runID, TenantID: "team", OwnerID: "alice",
		Status: store.RunStatusCancelled, Error: cancelReason,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	publishes := 0
	p := &Publisher{
		store:      st,
		publishRun: func(context.Context, *queue.RunMessage) error { publishes++; return nil },
	}
	wf := &ir.Workflow{Name: "auto_resume"}
	spec := runview.ResumeSpec{
		RunID: runID, FilePath: "auto_resume.bot",
		Source:    "workflow auto_resume:\n  entry: done\n",
		Automatic: true,
	}

	err = p.SubmitResume(ctx, spec, wf, "hash")
	if err == nil || !strings.Contains(err.Error(), "not auto-resumable") {
		t.Fatalf("SubmitResume(automatic, cancelled) = %v, want a refusal naming auto-resume", err)
	}
	if publishes != 0 {
		t.Fatalf("automatic resume of a cancelled run published %d message(s)", publishes)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusCancelled || r.Error != cancelReason {
		t.Fatalf("run doc must be untouched, got status=%s error=%q", r.Status, r.Error)
	}

	spec.Automatic = false
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err != nil {
		t.Fatalf("operator resume of a cancelled run must still proceed: %v", err)
	}
	if publishes != 1 {
		t.Fatalf("operator resume must publish once, got %d", publishes)
	}
}
