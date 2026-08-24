package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The operator's launch-time model/backend pins must ride the cloud wire
// AND land on the run doc: the runner builds its own executor, so a pin
// persisted display-only is an override the studio shows but the
// delegates never honour. Falsified both ways: entries travel to both
// carriers; an override-less launch publishes byte-identical messages
// (nil field); a resume replays the run doc's pins.

func TestSubmitLaunchCarriesModelOverrides(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   "workflow wf:\n  start -> done\n",
		ModelOverrides: []runview.ModelOverrideEntry{
			{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"},
			{Selector: "judge", Model: "claude-opus-5"},
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-mo-1", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil {
		t.Fatal("nothing published")
	}
	if len(published.ModelOverrides) != 2 ||
		published.ModelOverrides[0] != (queue.ModelOverride{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"}) ||
		published.ModelOverrides[1] != (queue.ModelOverride{Selector: "judge", Model: "claude-opus-5"}) {
		t.Fatalf("message overrides = %+v, want the two launch pins verbatim", published.ModelOverrides)
	}
	r, err := st.LoadRun(ctx, "run-mo-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(r.ModelOverrides) != 2 || r.ModelOverrides[0].Model != "claude-fable-5" || r.ModelOverrides[1].Selector != "judge" {
		t.Fatalf("run doc overrides = %+v, want the two pins persisted for display + resume replay", r.ModelOverrides)
	}
}

func TestSubmitLaunchWithoutOverridesPublishesNone(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}
	if _, err := p.SubmitLaunch(ctx, "run-mo-2", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil || published.ModelOverrides != nil {
		t.Fatalf("override-less launch published %+v, want nil (older consumers stay byte-identical)", published.ModelOverrides)
	}
	if r, _ := st.LoadRun(ctx, "run-mo-2"); r != nil && r.ModelOverrides != nil {
		t.Fatalf("override-less launch persisted %+v, want none", r.ModelOverrides)
	}
}

func TestSubmitResumeReplaysRunDocOverrides(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	const runID = "run-mo-resume"
	if err := st.SaveRun(ctx, &store.Run{
		ID:       runID,
		TenantID: "team-a",
		OwnerID:  "u1",
		Status:   store.RunStatusFailedResumable,
		ModelOverrides: []store.RunModelOverride{
			{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"},
		},
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil {
		t.Fatal("nothing published")
	}
	if len(published.ModelOverrides) != 1 ||
		published.ModelOverrides[0] != (queue.ModelOverride{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"}) {
		t.Fatalf("resume overrides = %+v, want the launch pin replayed from the run doc", published.ModelOverrides)
	}
}
