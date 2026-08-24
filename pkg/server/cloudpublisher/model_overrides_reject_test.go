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

// Issue #481: queue schema v7 cannot carry ModelOverrides, and a cloud run
// that launched anyway would execute WITHOUT the backend/model pins the
// operator explicitly chose — silently re-making operator intent on the
// runner pod. The publisher must reject such launches loudly (until schema
// v8 carries the field), not drop the overrides.
func TestSubmitLaunch_RejectsModelOverrides(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	published := false
	p := &Publisher{
		store:      st,
		publishRun: func(context.Context, *queue.RunMessage) error { published = true; return nil },
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	spec := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   "workflow wf:\n  start -> done\n",
		ModelOverrides: []runview.ModelOverrideEntry{
			{Selector: "reviewer_*", Model: "anthropic/claude-opus-4-8"},
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-mo", spec, &ir.Workflow{Name: "wf"}, "hash"); err == nil {
		t.Fatal("a cloud launch carrying model_overrides must be rejected, not silently stripped")
	} else if !strings.Contains(err.Error(), "model_overrides") || !strings.Contains(err.Error(), "#481") {
		t.Fatalf("error must name the field and the issue, got: %v", err)
	}
	if published {
		t.Fatal("a rejected launch must never reach the queue")
	}
	if _, err := st.LoadRun(ctx, "run-mo"); err == nil {
		t.Fatal("a rejected launch must leave NO run document behind")
	}
}

// The empty case must stay a no-op: a launch with no overrides expressed
// changes nothing about the operator's intent and flows through as before.
func TestSubmitLaunch_EmptyModelOverridesFlowsThrough(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{
		store:      st,
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}
	if _, err := p.SubmitLaunch(ctx, "run-ok", spec, &ir.Workflow{Name: "wf"}, "hash"); err != nil {
		t.Fatalf("empty model_overrides must not trip the rejection: %v", err)
	}
}
