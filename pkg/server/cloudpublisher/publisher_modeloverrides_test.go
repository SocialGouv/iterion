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

// TestSubmitResumeReplaysTheLaunchedModelOverrides pins the half the launch
// fix left open. In cloud a resume republishes a fresh RunMessage, and the
// runner builds its executor from THAT message — so a resume that omits the
// overrides hands the pod nothing and it silently falls back to the .bot's
// own model:.
//
// That is the common path, not a corner: a conversational run pauses on its
// chat node and every operator reply is a resume. Without this the chosen
// model held for exactly one turn while the studio header kept displaying it.
func TestSubmitResumeReplaysTheLaunchedModelOverrides(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	wf := &ir.Workflow{Name: "wf"}
	src := "workflow wf:\n  start -> done\n"

	// Launch with a choice, then resume the way a chat turn does.
	launch := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   src,
		ModelOverrides: []runview.ModelOverrideEntry{
			{Selector: "*", Model: "openai/gpt-5.5", Backend: "claw", Effort: "high"},
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-1", launch, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-1", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	published = nil

	if err := p.SubmitResume(ctx, runview.ResumeSpec{
		RunID:    "run-1",
		FilePath: "wf.bot",
		Source:   src,
		Answers:  map[string]any{"chat": "and then?"},
	}, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}

	if published == nil {
		t.Fatal("no RunMessage published on resume")
	}
	if len(published.ModelOverrides) != 1 {
		t.Fatalf("resume msg.ModelOverrides len = %d, want 1 — the pod would silently run the DSL default", len(published.ModelOverrides))
	}
	got := published.ModelOverrides[0]
	if got.Selector != "*" || got.Model != "openai/gpt-5.5" || got.Backend != "claw" || got.Effort != "high" {
		t.Errorf("resume msg.ModelOverrides[0] = %+v, want the launch's choice replayed verbatim", got)
	}
}

// A run launched without a choice must resume without one, so the runner's
// "nil means inherit the DSL" path keeps its meaning across a resume too.
func TestSubmitResumeWithoutModelOverridesStaysNil(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	wf := &ir.Workflow{Name: "wf"}
	src := "workflow wf:\n  start -> done\n"
	if _, err := p.SubmitLaunch(ctx, "run-2", runview.LaunchSpec{FilePath: "wf.bot", Source: src}, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-2", store.RunStatusFailedResumable, "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	published = nil
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: "run-2", FilePath: "wf.bot", Source: src}, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil {
		t.Fatal("no RunMessage published on resume")
	}
	if published.ModelOverrides != nil {
		t.Errorf("resume msg.ModelOverrides = %+v, want nil", published.ModelOverrides)
	}
}
