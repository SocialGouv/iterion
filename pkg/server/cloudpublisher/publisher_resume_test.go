package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestSubmitResume_PublishFailureRestoresOperatorPause(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-operator-paused"
	if err := st.SaveRun(ctx, &store.Run{
		ID:       runID,
		TenantID: "team",
		OwnerID:  "alice",
		Status:   store.RunStatusPausedOperator,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	p := &Publisher{
		store: st,
		publishRun: func(context.Context, *queue.RunMessage) error {
			return errors.New("nats unavailable")
		},
	}
	wf := &ir.Workflow{Name: "operator_resume"}
	spec := runview.ResumeSpec{
		RunID:    runID,
		FilePath: "operator_resume.bot",
		Source:   "workflow operator_resume:\n  entry: done\n",
	}

	err = p.SubmitResume(ctx, spec, wf, "hash")
	if err == nil {
		t.Fatal("SubmitResume returned nil, want publish failure")
	}
	if !strings.Contains(err.Error(), "nats unavailable") {
		t.Fatalf("SubmitResume error = %v, want publish failure", err)
	}

	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Fatalf("status after publish failure = %s, want paused_operator", r.Status)
	}
}
