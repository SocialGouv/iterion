package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #1959 — the resolved bot-source tier rides the wire on BOTH publications:
// the launch (from the resolver's answer) and the resume (replayed from the
// run document, never re-resolved), so the runner's isolation floor judges
// the same source on every attempt.
func TestSubmitLaunchAndResume_CarryTheBotSourceTier(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published []*queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, msg *queue.RunMessage) error {
			published = append(published, msg)
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team", "user")
	wf := &ir.Workflow{Name: "team_bot"}
	spec := runview.LaunchSpec{
		FilePath:      "team.bot",
		Source:        "workflow team_bot:\n  start -> done\n",
		BotID:         "team-bot",
		BotSourceTier: store.BotSourceTierTeam,
	}
	if _, err := p.SubmitLaunch(ctx, "run-tier", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-tier", store.RunStatusFailedResumable, "needs retry"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-tier", Source: spec.Source,
	}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d messages, want 2 (launch + resume)", len(published))
	}
	launch, resume := published[0], published[1]
	if launch.BotSourceTier != store.BotSourceTierTeam {
		t.Errorf("launch BotSourceTier = %q, want %q", launch.BotSourceTier, store.BotSourceTierTeam)
	}
	if resume.BotSourceTier != store.BotSourceTierTeam {
		t.Errorf("resume BotSourceTier = %q, want %q — the tier replays from the document like Trust does", resume.BotSourceTier, store.BotSourceTierTeam)
	}
	// The document itself carries the stamp both sides read.
	got, err := st.LoadRun(ctx, "run-tier")
	if err != nil {
		t.Fatal(err)
	}
	if got.BotSourceTier != store.BotSourceTierTeam {
		t.Errorf("run document BotSourceTier = %q, want %q", got.BotSourceTier, store.BotSourceTierTeam)
	}
}
