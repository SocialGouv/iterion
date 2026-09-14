package assistantmission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testMission(now time.Time) Mission {
	return Mission{
		Version: SchemaVersion, ID: "m1", InvocationKey: "goal:r1", TenantID: "t", ProjectID: "p", OperatorID: "o",
		TargetRunID: "r1", WatchID: "w1", AssistantRunID: "a1",
		Policy: Policy{Actions: []string{ActionRewind, ActionResume}, TTLSeconds: 600, ExpiresAt: now.Add(10 * time.Minute), MaxActions: 3, ContractVersion: ContractVersion},
		State:  StateActive, ProposalFrontier: map[string]int{}, CreatedAt: now, UpdatedAt: now,
	}
}

func TestFSStoreInvocationIsPermanentAndDoesNotRenewPolicy(t *testing.T) {
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	st := NewFSStore(t.TempDir())
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	original := testMission(now)
	created, fresh, err := st.CreateOrGet(ctx, original)
	if err != nil || !fresh {
		t.Fatalf("create = %#v, %v, %v", created, fresh, err)
	}
	stopped, err := st.RequestStop(ctx, Scope{TenantID: "t", OperatorID: "o", TargetRunID: "r1"}, original.ID, "done", now.Add(time.Minute))
	if err != nil || stopped.State != StateStopped {
		t.Fatalf("stop = %#v, %v", stopped, err)
	}
	retry := original
	retry.ID = "different"
	retry.Policy.ExpiresAt = now.Add(time.Hour)
	reattached, fresh, err := st.CreateOrGet(ctx, retry)
	if err != nil || fresh || reattached.ID != original.ID || reattached.Policy.ExpiresAt != original.Policy.ExpiresAt {
		t.Fatalf("reattach renewed or replaced mission: %#v, fresh=%v err=%v", reattached, fresh, err)
	}
}

func TestFSStoreRejectsConcurrentTargetAndFencesClaims(t *testing.T) {
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	st := NewFSStore(t.TempDir())
	first := testMission(now)
	if _, _, err := st.CreateOrGet(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := testMission(now)
	second.ID, second.InvocationKey, second.OperatorID = "m2", "other", "other-operator"
	if _, _, err := st.CreateOrGet(ctx, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("second active mission error = %v, want conflict", err)
	}
	claimed, won, err := st.Claim(ctx, first.ID, "worker-1", now, time.Minute)
	if err != nil || !won {
		t.Fatalf("claim = %#v, %v, %v", claimed, won, err)
	}
	if _, won, err := st.Claim(ctx, first.ID, "worker-2", now.Add(time.Second), time.Minute); err != nil || won {
		t.Fatalf("competing claim won=%v err=%v", won, err)
	}
	stale := claimed
	stale.LeaseEpoch--
	if _, err := st.UpdateClaimed(ctx, stale, "worker-1"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale update error = %v, want claim lost", err)
	}
}

func TestFSStorePersistsAcrossInstances(t *testing.T) {
	ctx, now, root := context.Background(), time.Now().UTC().Truncate(time.Millisecond), t.TempDir()
	first := NewFSStore(root)
	want := testMission(now)
	if _, _, err := first.CreateOrGet(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := NewFSStore(root).Get(ctx, Scope{TenantID: "t", OperatorID: "o", TargetRunID: "r1"}, want.ID)
	if err != nil || got.InvocationKey != want.InvocationKey {
		t.Fatalf("reopened get = %#v, %v", got, err)
	}
}
