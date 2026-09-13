package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

func TestAssistantWatchSweepDeliversBeyondAStoppedFirstPage(t *testing.T) {
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "paged-assistant", "unrelated-card")
	target := f.target(t, "paged-target", "target-card", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	for i := 0; i <= 500; i++ {
		targetID := fmt.Sprintf("missing-%03d", i)
		if i == 500 {
			targetID = target.ID
		}
		w := runwatch.Watch{ID: fmt.Sprintf("watch-%03d", i), TargetRunID: targetID, AssistantRunID: assistant.ID, State: runwatch.WatchActive, Kinds: []string{trigger.KindRunFailed}, CreatedAt: now.Add(time.Duration(i) * time.Nanosecond)}
		if err := f.ws.CreateWatch(t.Context(), w); err != nil {
			t.Fatal(err)
		}
	}
	deliveries := 0
	f.coord.resumeRun = func(context.Context, runview.ResumeSpec) (*runview.LaunchResult, error) {
		deliveries++
		return &runview.LaunchResult{}, nil
	}
	f.coord.sweep(t.Context())
	episodes, err := f.ws.ListEpisodesByWatch(t.Context(), "watch-500", "", 10)
	if err != nil || len(episodes) != 1 || episodes[0].State != runwatch.EpisodeDone || deliveries != 1 {
		t.Fatalf("last page was not delivered: %+v deliveries=%d err=%v", episodes, deliveries, err)
	}
	first, err := f.ws.GetWatch(t.Context(), "watch-000")
	if err != nil || first.State != runwatch.WatchStopped {
		t.Fatalf("fixture did not remove the first active page: %+v %v", first, err)
	}
	f.coord.sweep(t.Context())
	if deliveries != 1 {
		t.Fatal("pagination reconciliation redelivered the episode")
	}
}
