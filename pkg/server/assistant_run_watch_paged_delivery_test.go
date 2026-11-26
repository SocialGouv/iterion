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

// countedWatchPages counts the active-watch pages that returned rows, so a
// pagination test proves its sweep went beyond the first one.
type countedWatchPages struct {
	runwatch.Store
	pages int
}

func (s *countedWatchPages) ListActivePage(ctx context.Context, after *runwatch.WatchCursor, through runwatch.WatchCursor, limit int) ([]runwatch.Watch, error) {
	rows, err := s.Store.ListActivePage(ctx, after, through, limit)
	if len(rows) > 0 {
		s.pages++
	}
	return rows, err
}

// sweepWatchPage is the page size the pagination tests sweep with: the last
// of sweepWatchPage+1 watches sits alone on the second page. The sweep's own
// page is 500 rows, and the file store rewrites its whole state on every
// write, so 501 watches would cost about a minute per test under -race.
const sweepWatchPage = 5

// pageWatchesBy has f's coordinator sweep pages of size rows, its runs and
// catalog unchanged, and returns the page counter it reads them through.
func (f *armFixture) pageWatchesBy(size int) *countedWatchPages {
	paging := &countedWatchPages{Store: f.ws}
	current := f.coord.snapshot()
	f.coord.setRuntime(current.runs, paging, current.botPaths)
	f.coord.mu.Lock()
	f.coord.sweepPage = size
	f.coord.mu.Unlock()
	return paging
}

func TestAssistantWatchSweepDeliversBeyondAStoppedFirstPage(t *testing.T) {
	f := newArmFixture(t, true)
	paging := f.pageWatchesBy(sweepWatchPage)
	assistant := f.assistant(t, "paged-assistant", "unrelated-card")
	target := f.target(t, "paged-target", "target-card", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	for i := 0; i <= sweepWatchPage; i++ {
		targetID := fmt.Sprintf("missing-%03d", i)
		if i == sweepWatchPage {
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
	if paging.pages != 2 {
		t.Fatalf("the sweep read %d non-empty pages of %d watches, want 2", paging.pages, sweepWatchPage)
	}
	episodes, err := f.ws.ListEpisodesByWatch(t.Context(), fmt.Sprintf("watch-%03d", sweepWatchPage), "", 10)
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
