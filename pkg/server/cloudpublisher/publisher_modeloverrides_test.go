package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSubmitLaunchCarriesModelOverrides is the regression test for the
// operator's model choice being visible but inert in cloud mode: the queued
// doc stamped the override (so the studio Overview rendered it) while the
// RunMessage carried nothing, leaving the pod on the bot's DSL default. Both
// halves are asserted together — a run record that advertises a model the pod
// never used is worse than no override at all.
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
			{Selector: "assistant", Model: "anthropic/claude-opus-5", Backend: "claude_code", Effort: "ultracode"},
			{Selector: "judge", Provider: "anthropic"},
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-1", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}

	// Half 1: the queued doc, which the Overview reads.
	r, err := st.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(r.ModelOverrides) != 2 {
		t.Fatalf("run.ModelOverrides len = %d, want 2", len(r.ModelOverrides))
	}
	if got := r.ModelOverrides[0]; got.Model != "anthropic/claude-opus-5" || got.Backend != "claude_code" || got.Effort != "ultracode" {
		t.Errorf("run.ModelOverrides[0] = %+v", got)
	}

	// Half 2: the wire, which the runner pod reads. This is the assertion
	// that used to fail silently.
	if published == nil {
		t.Fatal("no RunMessage published")
	}
	if len(published.ModelOverrides) != 2 {
		t.Fatalf("msg.ModelOverrides len = %d, want 2 (the pod would run the DSL default)", len(published.ModelOverrides))
	}
	got := published.ModelOverrides[0]
	if got.Selector != "assistant" || got.Model != "anthropic/claude-opus-5" || got.Backend != "claude_code" || got.Effort != "ultracode" {
		t.Errorf("msg.ModelOverrides[0] = %+v", got)
	}
	if p := published.ModelOverrides[1]; p.Selector != "judge" || p.Provider != "anthropic" {
		t.Errorf("msg.ModelOverrides[1] = %+v", p)
	}
	if published.V != queue.SchemaVersion {
		t.Errorf("msg.V = %d, want %d", published.V, queue.SchemaVersion)
	}
}

// A launch with no picker choice must stay unstamped on both halves, so the
// runner's "nil means inherit the DSL" path keeps its meaning.
func TestSubmitLaunchWithoutModelOverridesStaysNil(t *testing.T) {
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
	if _, err := p.SubmitLaunch(ctx, "run-2", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	r, err := st.LoadRun(ctx, "run-2")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.ModelOverrides != nil {
		t.Errorf("run.ModelOverrides = %+v, want nil", r.ModelOverrides)
	}
	if published.ModelOverrides != nil {
		t.Errorf("msg.ModelOverrides = %+v, want nil", published.ModelOverrides)
	}
}
