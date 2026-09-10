package cloudpublisher

import (
	"context"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The operator's run-level fallback route must ride the cloud wire AND
// land on the run doc: the runner builds its own executor, so a route
// the server accepted but never published is a rescue the studio implies
// and the pod never takes — on exactly the path (unattended cloud runs
// hitting a provider's usage window) the route exists for. Falsified
// both ways: the route travels to both carriers; a routeless launch
// publishes byte-identical messages (nil field); a resume replays the
// run doc's route.

func TestSubmitLaunchCarriesRunFallback(t *testing.T) {
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
		Fallback: []runview.FallbackEntry{
			{Backend: "codex", Model: "gpt-5.5"},
			{Backend: "claw", Model: "openai/gpt-5.5"},
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-fb-1", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil {
		t.Fatal("nothing published")
	}
	wantQueue := queue.RunFallback{
		{Backend: "codex", Model: "gpt-5.5"},
		{Backend: "claw", Model: "openai/gpt-5.5"},
	}
	if !reflect.DeepEqual(published.Fallback, wantQueue) {
		t.Fatalf("message fallback = %+v, want the launch chain verbatim", published.Fallback)
	}
	r, err := st.LoadRun(ctx, "run-fb-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	wantStore := store.RunFallback{
		{Backend: "codex", Model: "gpt-5.5"},
		{Backend: "claw", Model: "openai/gpt-5.5"},
	}
	if !reflect.DeepEqual(r.Fallback, wantStore) {
		t.Fatalf("run doc fallback = %+v, want the chain persisted for the resume replay", r.Fallback)
	}
}

func TestSubmitLaunchWithoutFallbackPublishesNone(t *testing.T) {
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
	if _, err := p.SubmitLaunch(ctx, "run-fb-2", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil || published.Fallback != nil {
		t.Fatalf("routeless launch published %+v, want nil (older consumers stay byte-identical)", published.Fallback)
	}
	if r, _ := st.LoadRun(ctx, "run-fb-2"); r != nil && r.Fallback != nil {
		t.Fatalf("routeless launch persisted %+v, want none", r.Fallback)
	}
}

func TestSubmitResumeReplaysRunDocFallback(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	const runID = "run-fb-resume"
	if err := st.SaveRun(ctx, &store.Run{
		ID:       runID,
		TenantID: "team-a",
		OwnerID:  "u1",
		Status:   store.RunStatusFailedResumable,
		Fallback: store.RunFallback{
			{Backend: "codex", Model: "gpt-5.5"},
			{Backend: "claw", Model: "openai/gpt-5.5"},
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
	want := queue.RunFallback{
		{Backend: "codex", Model: "gpt-5.5"},
		{Backend: "claw", Model: "openai/gpt-5.5"},
	}
	if !reflect.DeepEqual(published.Fallback, want) {
		t.Fatalf("resume fallback = %+v, want the launch chain replayed from the run doc — the auto-retry after a usage-window park is exactly the publication that must still carry the rescue", published.Fallback)
	}
}
