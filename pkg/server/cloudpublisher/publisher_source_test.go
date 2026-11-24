package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSubmitLaunchStampsSourceRef is the regression test for cloud runs
// losing their typed provenance: the queued doc is the only carrier
// (RunMessage has no source field, and the runner's engine never invents
// one), so dropping spec.SourceRef made every scheduled / dispatcher /
// spine run read as source_kind "manual" AND made the overlap gate — which
// counts a schedule's live runs through source.schedule_id — see zero.
func TestSubmitLaunchStampsSourceRef(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{
		store:      st,
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "scheduler:vigie")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   "workflow wf:\n  start -> done\n",
		SourceRef: &store.RunSource{
			Kind:         store.RunSourceKindSchedule,
			ScheduleID:   "sched-42",
			ScheduleName: "feed-watch",
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-1", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	r, err := st.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Source == nil {
		t.Fatal("run.Source = nil, want the schedule provenance")
	}
	if r.Source.Kind != store.RunSourceKindSchedule || r.Source.ScheduleID != "sched-42" || r.Source.ScheduleName != "feed-watch" {
		t.Fatalf("run.Source = %+v, want {schedule sched-42 feed-watch}", r.Source)
	}
	// The persisted copy must be independent of the caller's pointer.
	spec.SourceRef.ScheduleID = "mutated"
	if r.Source.ScheduleID == "mutated" {
		t.Fatal("run.Source aliases the caller's RunSource pointer")
	}
}

// The webhook delivery rides the same single carrier (#1501 unit 1501.3):
// the queued doc. A generic copy is not proof that a NEW RunSource field
// survives it — the run-outcome spine's delivery sink can only find its row
// through source.delivery_id, so the field is pinned where it crosses.
func TestSubmitLaunchStampsTheWebhookDelivery(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{
		store:      st,
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "webhook:hook")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{
		FilePath: "wf.bot",
		Source:   "workflow wf:\n  start -> done\n",
		SourceRef: &store.RunSource{
			Kind:       store.RunSourceKindWebhook,
			DeliveryID: "d-42",
		},
	}
	if _, err := p.SubmitLaunch(ctx, "run-3", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	r, err := st.LoadRun(ctx, "run-3")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Source == nil || r.Source.Kind != store.RunSourceKindWebhook || r.Source.DeliveryID != "d-42" {
		t.Fatalf("run.Source = %+v, want {webhook d-42}", r.Source)
	}
}

// A plain operator launch carries no SourceRef and must stay unstamped —
// deriveSourceKind's "manual" default depends on a nil Source.
func TestSubmitLaunchWithoutSourceRefLeavesSourceNil(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{
		store:      st,
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n"}
	if _, err := p.SubmitLaunch(ctx, "run-2", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	r, err := st.LoadRun(ctx, "run-2")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Source != nil {
		t.Fatalf("run.Source = %+v, want nil for a manual launch", r.Source)
	}
}
