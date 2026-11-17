package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The launch-time codemode override (CM-5e, #3345) rides the wire on BOTH
// publications and is stamped on the run document — the doc being the only
// carrier a resume surface that cannot re-state an override (the studio's
// answers, the retry sweeper) has. A publisher that drops either half runs
// the authored value over the operator's explicit choice, the direction the
// v33 bump exists to reject.
func TestSubmitLaunchAndResumeCarryTheCodemodeOverride(t *testing.T) {
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
	bounds := &scripthost.Limits{ToolCalls: 4096, Wall: 90 * time.Second}
	spec := runview.LaunchSpec{
		FilePath:       "team.bot",
		Source:         "workflow team_bot:\n  start -> done\n",
		BotID:          "team-bot",
		Codemode:       "off",
		CodemodeBounds: bounds,
	}
	if _, err := p.SubmitLaunch(ctx, "run-codemode", spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, "run-codemode", store.RunStatusFailedResumable, "needs retry"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-codemode", Source: spec.Source,
	}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d messages, want 2 (launch + resume)", len(published))
	}
	launch, resume := published[0], published[1]
	if launch.Codemode != "off" || launch.CodemodeBounds == nil || launch.CodemodeBounds.ToolCalls != 4096 {
		t.Errorf("launch carried Codemode=%q bounds=%+v, want the spec's override", launch.Codemode, launch.CodemodeBounds)
	}
	// The RESUME message replays the launch's choice — from the run doc the
	// launch stamped, because a bare ResumeSpec carries no codemode.
	if resume.Codemode != "off" || resume.CodemodeBounds == nil || resume.CodemodeBounds.ToolCalls != 4096 {
		t.Errorf("resume carried Codemode=%q bounds=%+v, want the run doc's replay of the launch's choice", resume.Codemode, resume.CodemodeBounds)
	}
	// And the doc itself carries the stamp, which is what makes every OTHER
	// resume surface (sweeper, WS answers) able to replay it.
	doc, err := st.LoadRun(ctx, "run-codemode")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if doc.CodemodeOverride != "off" || doc.CodemodeBoundsOverride == nil || doc.CodemodeBoundsOverride.ToolCalls != 4096 {
		t.Errorf("run doc CodemodeOverride=%q bounds=%+v, want the launch's override stamped", doc.CodemodeOverride, doc.CodemodeBoundsOverride)
	}
}
