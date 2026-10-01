package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSubmitResume_aResumeInsideTheFlipMillisecondKeepsItsAttempt: resume A
// flips the run to queued and is slow to publish; in its window the run is
// cancelled and resumed again (B), which publishes — inside the very
// millisecond A's flip stamped. A's marker follows the attempt it replaced,
// so B's is a different one, and A's rollback — a match on A's own marker —
// leaves B's attempt as B left it.
func TestSubmitResume_aResumeInsideTheFlipMillisecondKeepsItsAttempt(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-flip-ms-race"
	prior := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusPausedWaitingHuman, QueuedAt: &prior}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	aInPublish := make(chan struct{})
	releaseA := make(chan struct{})
	var mu sync.Mutex
	var publishedB *queue.RunMessage
	p := &Publisher{
		store:              st,
		publishRetryDelays: []time.Duration{},
		cancelRun:          func(string) error { return nil },
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			if m.Resume != nil && m.Resume.PriorStatus == store.RunStatusPausedWaitingHuman {
				close(aInPublish)
				<-releaseA
				return errors.New("nats: publish ack timeout (A)")
			}
			mu.Lock()
			publishedB = m
			mu.Unlock()
			return nil
		},
	}
	cs := rollbackTestSource()
	aErr := make(chan error, 1)
	go func() {
		aErr <- p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: cs.Files["main.bot"], Answers: map[string]any{"approve": true}}, &ir.Workflow{Name: "w"}, cs)
	}()
	<-aInPublish
	// No spacing: the cancel and B's resume race A's flip for the same
	// millisecond. The marker follows the attempt it replaces, so they
	// cannot share one.
	if err := p.CancelRun(ctx, runID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	csB := &runview.CompiledSource{Hash: "hB", Main: "main.bot", Files: map[string]string{"main.bot": "workflow w:\n  entry: b\n  b -> done\n"}}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: csB.Files["main.bot"], AcceptScratchLoss: true}, &ir.Workflow{Name: "w"}, csB); err != nil {
		t.Fatalf("B's resume = %v, want success", err)
	}
	afterB, err := st.LoadRun(ctx, runID)
	if err != nil || afterB.QueuedAt == nil {
		t.Fatalf("after B: %v (%v)", afterB, err)
	}
	close(releaseA)
	if err := <-aErr; err == nil || !strings.Contains(err.Error(), "(A)") {
		t.Fatalf("A = %v, want its publish failure", err)
	}
	final, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	published, perr := time.Parse(time.RFC3339Nano, publishedB.PublishedAtRFC)
	if perr != nil {
		t.Fatal(perr)
	}
	if final.Status != store.RunStatusQueued || final.QueuedAt == nil || !final.QueuedAt.Equal(*afterB.QueuedAt) || final.QueuedAt.After(published) {
		t.Fatalf("A's rollback reverted B's published attempt: status %s queued_at %v, want queued at B's marker %v (B published at %s)", final.Status, final.QueuedAt, afterB.QueuedAt, publishedB.PublishedAtRFC)
	}
	if final.WorkflowHash != "hB" || !strings.Contains(final.WorkflowSource, "entry: b") {
		t.Fatalf("A's rollback put back the rewind baseline over B's: %q (%s)", final.WorkflowSource, final.WorkflowHash)
	}
}

// TestSubmitResume_thePublicationNeverPrecedesItsOwnMarker: a message's
// published_at is never before the marker of the attempt it publishes — the
// attempt's own delivery must not read as superseded by it.
func TestSubmitResume_thePublicationNeverPrecedesItsOwnMarker(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-publish-after-marker"
	// A marker in the future of this test's clock: the flip must follow the
	// attempt it replaces, and the publication must follow its own flip.
	marker := time.Now().UTC().Add(50 * time.Millisecond).Truncate(time.Millisecond)
	if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusPausedOperator, QueuedAt: &marker}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{store: st, publishRun: func(_ context.Context, m *queue.RunMessage) error {
		published = m
		return nil
	}}
	cs := rollbackTestSource()
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: cs.Files["main.bot"]}, &ir.Workflow{Name: "w"}, cs); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil {
		t.Fatal("the resume published nothing")
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil || run.QueuedAt == nil {
		t.Fatalf("the resumed run: %v (%v)", run, err)
	}
	pub, perr := time.Parse(time.RFC3339Nano, published.PublishedAtRFC)
	if perr != nil {
		t.Fatal(perr)
	}
	if run.QueuedAt.After(pub) {
		t.Fatalf("the publication (%s) precedes its own attempt's marker (%v): the delivery would read as superseded and be dropped", published.PublishedAtRFC, run.QueuedAt)
	}
	if queue.Superseded(published, run) {
		t.Fatalf("the attempt's own delivery reads as superseded: marker %v, published_at %s", run.QueuedAt, published.PublishedAtRFC)
	}
}
