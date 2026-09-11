package runwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFSStoreWatchEpisodeLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	w := Watch{ID: "w1", TenantID: "t1", OwnerID: "u1", TargetRunID: "target", AssistantRunID: "assistant", Mode: ModeDiagnose, State: WatchActive, MaxEpisodes: 3, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	duplicate := w
	duplicate.ID = "w2"
	if err := s.CreateWatch(ctx, duplicate); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate = %v", err)
	}
	ep := Episode{ID: "ep1", WatchID: w.ID, TenantID: w.TenantID, TargetRunID: w.TargetRunID, AssistantRunID: w.AssistantRunID, OutcomeEventID: "event1", FailureFingerprint: "fp", State: EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	created, err := s.CreateEpisode(ctx, ep)
	if err != nil || !created {
		t.Fatalf("create episode = %v, %v", created, err)
	}
	created, err = s.CreateEpisode(ctx, ep)
	if err != nil || created {
		t.Fatalf("duplicate episode = %v, %v", created, err)
	}

	// Concurrent replicas compete for one CAS lease; exactly one may deliver.
	var wg sync.WaitGroup
	winners := make(chan Episode, 2)
	for _, owner := range []string{"pod-a", "pod-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			got, won, claimErr := s.ClaimEpisode(ctx, ep.ID, owner, now, time.Minute)
			if claimErr != nil {
				t.Errorf("claim %s: %v", owner, claimErr)
				return
			}
			if won {
				winners <- got
			}
		}(owner)
	}
	wg.Wait()
	close(winners)
	var won []Episode
	for ep := range winners {
		won = append(won, ep)
	}
	if len(won) != 1 {
		t.Fatalf("winners = %d, want 1", len(won))
	}
	if err := s.CompleteEpisode(ctx, ep.ID, won[0].LeaseOwner, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWatch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveredEpisodes != 1 {
		t.Fatalf("delivered episodes = %d", got.DeliveredEpisodes)
	}
}

func TestFSStoreExpiredLeaseCanBeReclaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	now := time.Now().UTC()
	ep := Episode{ID: "ep", WatchID: "w", State: EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	if _, err := s.CreateEpisode(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if _, won, err := s.ClaimEpisode(ctx, ep.ID, "dead-pod", now, time.Second); err != nil || !won {
		t.Fatalf("first claim = %v, %v", won, err)
	}
	if _, won, err := s.ClaimEpisode(ctx, ep.ID, "new-pod", now.Add(2*time.Second), time.Minute); err != nil || !won {
		t.Fatalf("reclaim = %v, %v", won, err)
	}
}

func TestFSStoreAdvanceObservedEventSeqIsMonotonic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	now := time.Now().UTC()
	w := Watch{ID: "w", TenantID: "t", TargetRunID: "target", AssistantRunID: "assistant", State: WatchActive, LastObservedEventSeq: -1, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceObservedEventSeq(ctx, w.ID, w.TenantID, 9, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceObservedEventSeq(ctx, w.ID, w.TenantID, 4, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWatch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastObservedEventSeq != 9 {
		t.Fatalf("cursor = %d, want 9", got.LastObservedEventSeq)
	}
}

func TestFSStoreTreeTrackingAndPerRunCursorAreMonotonic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	created := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	w := Watch{ID: "tree", TenantID: "tenant", TargetRunID: "root", AssistantRunID: "assistant", State: WatchActive, LastObservedEventSeq: 7, CreatedAt: created, UpdatedAt: created}
	if err := s.CreateWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	firstStamp := created.Add(time.Hour)
	got, err := s.InitializeTreeTracking(ctx, w.ID, w.TenantID, firstStamp, firstStamp)
	if err != nil {
		t.Fatal(err)
	}
	if got.TreeTrackingStartedAt == nil || !got.TreeTrackingStartedAt.Equal(firstStamp) {
		t.Fatalf("tracking stamp = %v, want %v", got.TreeTrackingStartedAt, firstStamp)
	}
	secondStamp := firstStamp.Add(time.Hour)
	got, err = s.InitializeTreeTracking(ctx, w.ID, w.TenantID, secondStamp, secondStamp)
	if err != nil || got.TreeTrackingStartedAt == nil || !got.TreeTrackingStartedAt.Equal(firstStamp) {
		t.Fatalf("second initialization changed stamp: watch=%+v err=%v", got, err)
	}

	seed := RunObservation{RunID: "child", EventSeq: -1, FirstObservedAt: firstStamp}
	if persisted, created, err := s.EnsureRunObservation(ctx, w.ID, w.TenantID, seed, firstStamp); err != nil || !created || persisted.EventSeq != -1 {
		t.Fatalf("ensure observation = %+v created=%v err=%v", persisted, created, err)
	}
	competing := RunObservation{RunID: "child", EventSeq: 99, FirstObservedAt: secondStamp}
	if persisted, created, err := s.EnsureRunObservation(ctx, w.ID, w.TenantID, competing, secondStamp); err != nil || created || persisted.EventSeq != -1 || !persisted.FirstObservedAt.Equal(firstStamp) {
		t.Fatalf("competing observation replaced seed: %+v created=%v err=%v", persisted, created, err)
	}
	if err := s.AdvanceObservedRunEventSeq(ctx, w.ID, w.TenantID, "child", 12, secondStamp); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceObservedRunEventSeq(ctx, w.ID, w.TenantID, "child", 3, secondStamp.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetWatch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) != 1 || got.Observations[0].EventSeq != 12 {
		t.Fatalf("observations = %+v, want child cursor 12", got.Observations)
	}
	if got.LastObservedEventSeq != 7 {
		t.Fatalf("child cursor changed legacy root cursor to %d", got.LastObservedEventSeq)
	}
}

func TestFSStoreReconfigureActiveWatchPreservesDeliveryLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	created := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	delivered := created.Add(time.Minute)
	original := Watch{
		ID: "watch-original", TenantID: "tenant", OwnerID: "owner", TargetRunID: "target", AssistantRunID: "assistant",
		Mode: ModeDiagnose, Kinds: []string{"run.failed"}, State: WatchActive,
		MaxEpisodes: 3, DeliveredEpisodes: 2, CooldownSeconds: 30, LastDeliveredAt: &delivered,
		LastObservedEventSeq: 41, TreeTrackingStartedAt: &created,
		Observations: []RunObservation{{RunID: "child", EventSeq: 9, FirstObservedAt: created}},
		CreatedAt:    created, UpdatedAt: created,
	}
	if err := s.CreateWatch(ctx, original); err != nil {
		t.Fatal(err)
	}
	requested := Watch{
		ID: "ignored-new-id", TenantID: original.TenantID, OwnerID: original.OwnerID,
		TargetRunID: original.TargetRunID, AssistantRunID: original.AssistantRunID,
		Mode: ModePropose, Kinds: []string{"run.failed", "run.stalled"}, State: WatchActive,
		MaxEpisodes: 20, CooldownSeconds: 0, UpdatedAt: created.Add(2 * time.Minute),
	}
	updated, found, err := s.ReconfigureActiveWatch(ctx, requested)
	if err != nil || !found {
		t.Fatalf("reconfigure = found:%v err:%v", found, err)
	}
	if updated.ID != original.ID || updated.CreatedAt != original.CreatedAt || updated.DeliveredEpisodes != original.DeliveredEpisodes || updated.LastObservedEventSeq != original.LastObservedEventSeq || updated.LastDeliveredAt == nil || !updated.LastDeliveredAt.Equal(delivered) {
		t.Fatalf("delivery ledger changed: %+v", updated)
	}
	if updated.TreeTrackingStartedAt == nil || !updated.TreeTrackingStartedAt.Equal(created) || len(updated.Observations) != 1 || updated.Observations[0].EventSeq != 9 {
		t.Fatalf("tree tracking state changed: %+v", updated)
	}
	if updated.Mode != ModePropose || updated.MaxEpisodes != 0 || updated.CooldownSeconds != 0 || len(updated.Kinds) != 2 || updated.Kinds[1] != "run.stalled" {
		t.Fatalf("configuration was not updated: %+v", updated)
	}
	requested.AssistantRunID = "other-assistant"
	if _, found, err := s.ReconfigureActiveWatch(ctx, requested); err != nil || found {
		t.Fatalf("mismatched assistant = found:%v err:%v, want not found", found, err)
	}
}

func TestFSStoreTransferActiveWatchPreservesDeliveryLedgerAndUsesOutgoingCAS(t *testing.T) {
	ctx := context.Background()
	s := NewFSStore(t.TempDir())
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	delivered := created.Add(10 * time.Minute)
	original := Watch{
		ID: "watch-transfer", TenantID: "tenant", OwnerID: "owner", TargetRunID: "target",
		AssistantRunID: "outgoing", Mode: ModeDiagnose, Kinds: []string{"run.failed"}, State: WatchActive,
		DeliveredEpisodes: 3, LastDeliveredAt: &delivered, LastObservedEventSeq: 9,
		TreeTrackingStartedAt: &created,
		Observations:          []RunObservation{{RunID: "target", EventSeq: 9, FirstObservedAt: created}},
		CreatedAt:             created, UpdatedAt: created,
	}
	if err := s.CreateWatch(ctx, original); err != nil {
		t.Fatal(err)
	}
	requested := Watch{
		TenantID: original.TenantID, OwnerID: original.OwnerID, TargetRunID: original.TargetRunID,
		AssistantRunID: "incoming", Mode: ModePropose, Kinds: []string{"run.failed", "run.stalled"},
		CooldownSeconds: 0, UpdatedAt: created.Add(time.Minute),
	}
	updated, found, err := s.TransferActiveWatch(ctx, "outgoing", requested)
	if err != nil || !found {
		t.Fatalf("transfer = found:%v err:%v", found, err)
	}
	if updated.ID != original.ID || updated.AssistantRunID != "incoming" || updated.State != WatchActive {
		t.Fatalf("identity or active ownership changed: %+v", updated)
	}
	if updated.DeliveredEpisodes != original.DeliveredEpisodes || updated.LastObservedEventSeq != original.LastObservedEventSeq || updated.LastDeliveredAt == nil || !updated.LastDeliveredAt.Equal(delivered) {
		t.Fatalf("delivery ledger changed: %+v", updated)
	}
	if updated.TreeTrackingStartedAt == nil || !updated.TreeTrackingStartedAt.Equal(created) || len(updated.Observations) != 1 || updated.Observations[0].EventSeq != 9 {
		t.Fatalf("tree tracking changed: %+v", updated)
	}
	if updated.Mode != ModePropose || len(updated.Kinds) != 2 || updated.Kinds[1] != "run.stalled" {
		t.Fatalf("requested policy was not applied: %+v", updated)
	}
	if _, found, err := s.TransferActiveWatch(ctx, "outgoing", requested); err != nil || found {
		t.Fatalf("stale outgoing CAS = found:%v err:%v, want not found", found, err)
	}
}
