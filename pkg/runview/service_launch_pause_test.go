package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A human entry node pauses the engine immediately — no LLM, no tools —
// which exercises two Launch-path guarantees end to end:
//   - the run doc is persisted BEFORE Launch returns (a GET right after
//     the launch response must not 404 on the goroutine still booting);
//   - a pause keeps the run's WS subscribers registered (the resume's
//     engine publishes to the same broker runID; dropping them loses the
//     pause/resume events and the human-gate form lags until a reload).
const pausingBot = `
schema gate_out:
  approve: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow pause_demo:
  entry: gate
  gate -> done when approve
  gate -> fail when not approve
`

func TestLaunch_PersistsRunBeforeReturn_AndPauseKeepsSubscribers(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "pause_demo.bot")
	if err := os.WriteFile(botPath, []byte(pausingBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// (B) run.json must already exist — no retry, no sleep.
	if _, err := svc.store.LoadRun(context.Background(), res.RunID); err != nil {
		t.Fatalf("LoadRun immediately after Launch: %v", err)
	}

	sub := svc.broker.Subscribe(res.RunID)
	defer sub.Cancel()

	select {
	case <-res.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("run goroutine did not exit (expected immediate human pause)")
	}

	r, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("LoadRun after pause: %v", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status = %q, want %q", r.Status, store.RunStatusPausedWaitingHuman)
	}

	// (C) the pause exit must NOT have closed the run's subscriptions.
	if n := svc.broker.SubscriberCount(res.RunID); n != 1 {
		t.Errorf("SubscriberCount after pause = %d, want 1 (subscribers must survive a pause)", n)
	}
}
