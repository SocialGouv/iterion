package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestSubmitLaunchCarriesPermissionOverride(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = msg
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf", Permission: "off"}
	spec := runview.LaunchSpec{
		FilePath:   "wf.bot",
		Source:     "workflow wf:\n  start -> done\n",
		Permission: "deny",
	}
	if _, err := p.SubmitLaunch(ctx, "run-permission", spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}

	r, err := st.LoadRun(ctx, "run-permission")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.PermissionOverride != "deny" || r.PermissionMode != "deny" {
		t.Errorf("persisted permission = override %q / effective %q, want deny / deny", r.PermissionOverride, r.PermissionMode)
	}
	if published == nil {
		t.Fatal("no RunMessage published")
	}
	if published.Permission != "deny" {
		t.Fatalf("published Permission = %q, want deny", published.Permission)
	}
	if published.V != queue.SchemaVersion {
		t.Errorf("published schema = %d, want %d", published.V, queue.SchemaVersion)
	}
}

func TestSubmitResumeReplaysPermissionOverride(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = msg
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := &ir.Workflow{Name: "wf"}
	source := "workflow wf:\n  start -> done\n"
	if _, err := p.SubmitLaunch(ctx, "run-permission-resume", runview.LaunchSpec{
		FilePath: "wf.bot", Source: source, Permission: "ask",
	}, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-permission-resume", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	published = nil

	if err := p.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-permission-resume", FilePath: "wf.bot", Source: source,
		Answers: map[string]any{"message": "continue"},
	}, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil {
		t.Fatal("no RunMessage published on resume")
	}
	if published.Permission != "ask" {
		t.Fatalf("resume Permission = %q, want original ask", published.Permission)
	}
}
