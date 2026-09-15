package runwatch

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestWatchStoreReconciliationAndCompletion(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(*testing.T) Store
	}{
		{"fs", func(t *testing.T) Store { return NewFSStore(t.TempDir()) }},
		{"mongo", func(t *testing.T) Store { return newTestMongoWatchStore(t) }},
	} {
		t.Run(backend.name, func(t *testing.T) {
			t.Run("bounded pages survive ties and removals", func(t *testing.T) {
				s := backend.open(t)
				ctx := t.Context()
				now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
				if upper, err := s.ActiveWatchUpperBound(ctx); err != nil || upper != nil {
					t.Fatalf("empty bound=%+v err=%v", upper, err)
				}
				for i := 502; i >= 0; i-- {
					w := Watch{ID: fmt.Sprintf("w%03d", i), TenantID: "t", TargetRunID: fmt.Sprint(i), State: WatchActive, CreatedAt: now, UpdatedAt: now}
					if err := s.CreateWatch(ctx, w); err != nil {
						t.Fatal(err)
					}
				}
				upper, err := s.ActiveWatchUpperBound(ctx)
				if err != nil || upper == nil || upper.ID != "w502" {
					t.Fatalf("upper=%+v err=%v", upper, err)
				}
				first, err := s.ListActivePage(ctx, nil, *upper, 500)
				if err != nil || len(first) != 500 || first[0].ID != "w000" || first[499].ID != "w499" {
					t.Fatalf("first page count=%d err=%v", len(first), err)
				}
				// Remove the cursor row and a yet-unseen row from the active set.
				for _, id := range []string{"w499", "w500"} {
					if err := s.StopWatch(ctx, id, "t", WatchStopped, "test", now); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.CreateWatch(ctx, Watch{ID: "w999", TenantID: "t", TargetRunID: "new", State: WatchActive, CreatedAt: now.Add(time.Second), UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				after := watchCursor(first[499])
				second, err := s.ListActivePage(ctx, &after, *upper, 500)
				if err != nil || len(second) != 2 || second[0].ID != "w501" || second[1].ID != "w502" {
					t.Fatalf("second page=%+v err=%v", second, err)
				}
				after = watchCursor(second[1])
				if rest, err := s.ListActivePage(ctx, &after, *upper, 500); err != nil || len(rest) != 0 {
					t.Fatalf("pass exceeded bound: %+v err=%v", rest, err)
				}
				upper, err = s.ActiveWatchUpperBound(ctx)
				if err != nil || upper == nil || upper.ID != "w999" {
					t.Fatalf("next pass lost new watch: %+v err=%v", upper, err)
				}
			})
			t.Run("completion is fenced and delivery time never regresses", func(t *testing.T) {
				s := backend.open(t)
				ctx := t.Context()
				now := time.Now().UTC().Truncate(time.Millisecond)
				w, ep := seedWatchEpisode(t, s, now, "w", "t")
				if _, won, err := s.ClaimEpisode(ctx, ep.ID, "owner", now, time.Minute); err != nil || !won {
					t.Fatalf("claim won=%v err=%v", won, err)
				}
				if err := s.CompleteEpisode(ctx, ep.ID, "stale", now); !errors.Is(err, ErrNotFound) {
					t.Fatalf("stale owner = %v", err)
				}
				results := make(chan error, 2)
				var wg sync.WaitGroup
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); results <- s.CompleteEpisode(ctx, ep.ID, "owner", now.Add(2*time.Second)) }()
				}
				wg.Wait()
				close(results)
				wins := 0
				for err := range results {
					if err == nil {
						wins++
					} else if !errors.Is(err, ErrNotFound) {
						t.Fatal(err)
					}
				}
				if wins != 1 {
					t.Fatalf("completion winners=%d", wins)
				}
				ep.ID, ep.OutcomeEventID = "earlier-episode", "earlier-event"
				if _, err := s.CreateEpisode(ctx, ep); err != nil {
					t.Fatal(err)
				}
				if _, won, err := s.ClaimEpisode(ctx, ep.ID, "other", now, time.Minute); err != nil || !won {
					t.Fatalf("second claim won=%v err=%v", won, err)
				}
				if err := s.CompleteEpisode(ctx, ep.ID, "other", now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				got, err := s.GetWatch(ctx, w.ID)
				if err != nil || got.DeliveredEpisodes != 2 || got.LastDeliveredAt == nil || !got.LastDeliveredAt.Equal(now.Add(2*time.Second)) {
					t.Fatalf("accounting=%+v err=%v", got, err)
				}
				for i := range 2 {
					next := ep
					next.ID, next.OutcomeEventID = fmt.Sprintf("concurrent-%d", i), fmt.Sprintf("concurrent-event-%d", i)
					if _, err := s.CreateEpisode(ctx, next); err != nil {
						t.Fatal(err)
					}
					if _, won, err := s.ClaimEpisode(ctx, next.ID, "parallel", now, time.Minute); err != nil || !won {
						t.Fatalf("claim=%v err=%v", won, err)
					}
				}
				parallel := make(chan error, 2)
				for i := range 2 {
					go func() {
						parallel <- s.CompleteEpisode(ctx, fmt.Sprintf("concurrent-%d", i), "parallel", now.Add(time.Duration(3+i)*time.Second))
					}()
				}
				for range 2 {
					if err := <-parallel; err != nil {
						t.Fatal(err)
					}
				}
				got, err = s.GetWatch(ctx, w.ID)
				if err != nil || got.DeliveredEpisodes != 4 || got.LastDeliveredAt == nil || !got.LastDeliveredAt.Equal(now.Add(4*time.Second)) {
					t.Fatalf("concurrent accounting=%+v err=%v", got, err)
				}

			})
			for _, kind := range []string{"missing", "foreign", "local"} {
				t.Run("watch binding "+kind, func(t *testing.T) {
					s := backend.open(t)
					ctx := t.Context()
					now := time.Now().UTC().Truncate(time.Millisecond)
					tenant := "t"
					if kind == "local" {
						tenant = ""
					}
					_, ep := seedWatchEpisode(t, s, now, "w", tenant)
					if kind != "local" {
						ep.ID, ep.OutcomeEventID = kind, kind
						if kind == "missing" {
							ep.WatchID = "missing-watch"
						} else {
							ep.TenantID = "foreign"
						}
						if _, err := s.CreateEpisode(ctx, ep); err != nil {
							t.Fatal(err)
						}
					}
					if _, won, err := s.ClaimEpisode(ctx, ep.ID, "owner", now, time.Minute); err != nil || !won {
						t.Fatalf("claim won=%v err=%v", won, err)
					}
					err := s.CompleteEpisode(ctx, ep.ID, "owner", now)
					if kind == "local" {
						if err != nil {
							t.Fatal(err)
						}
						return
					}
					if !errors.Is(err, ErrNotFound) {
						t.Fatalf("invalid watch binding = %v", err)
					}
					if _, won, err := s.ClaimEpisode(ctx, ep.ID, "next-owner", now.Add(time.Minute), time.Minute); err != nil || !won {
						t.Fatalf("failed completion stranded episode: won=%v err=%v", won, err)
					}
					if _, err := s.GetWatch(ctx, ""); !errors.Is(err, ErrNotFound) {
						t.Fatalf("fabricated empty watch: %v", err)
					}
				})
			}
		})
	}
}

func seedWatchEpisode(t *testing.T, s Store, now time.Time, id, tenant string) (Watch, Episode) {
	t.Helper()
	w := Watch{ID: id, TenantID: tenant, OwnerID: "operator", TargetRunID: "target:" + id, AssistantRunID: "assistant", State: WatchActive, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateWatch(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	ep := Episode{ID: "ep:" + id, WatchID: w.ID, TenantID: tenant, TargetRunID: w.TargetRunID, AssistantRunID: w.AssistantRunID, OutcomeEventID: "event:" + id, State: EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	if _, err := s.CreateEpisode(t.Context(), ep); err != nil {
		t.Fatal(err)
	}
	return w, ep
}
