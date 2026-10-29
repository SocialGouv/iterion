package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #1959, the in-process half. The tier the bot resolver served reached the
// EXECUTOR (ExecutorSpec.BotSourceTier, so the launch's own predictions judge
// the floored workflow) and stopped there: the run DOCUMENT never recorded
// it. A resume rebuilds the engine from that document, reads an empty tier as
// the operator's own launch, and hands a team-authored `sandbox: none` back —
// the floor undone on the second attempt, silently. The same trap WithTrust
// closed for trust, except that trust is refused by this lane while the tier
// is simply carried.
func TestLaunch_TheInProcessLaneStampsTheBotSourceTierOnTheDocument(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithStore(st), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	res, err := svc.Launch(ctx, LaunchSpec{
		FilePath:      botPath,
		RunID:         "tier-stamped",
		BotSourceTier: store.BotSourceTierTeam,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	<-res.Done

	run, err := st.LoadRun(ctx, "tier-stamped")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.BotSourceTier != store.BotSourceTierTeam {
		t.Errorf("run.BotSourceTier = %q, want %q — a resume of this run would read the empty tier as the operator's own and honour the bot's sandbox opt-out", run.BotSourceTier, store.BotSourceTierTeam)
	}
}
