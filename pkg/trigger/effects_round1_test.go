package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// failingSubs wraps a SubscriptionStore with an injectable Get fault.
type failingSubs struct {
	SubscriptionStore
	getErr error
}

func (f *failingSubs) Get(ctx context.Context, id string) (Subscription, error) {
	if f.getErr != nil {
		return Subscription{}, f.getErr
	}
	return f.SubscriptionStore.Get(ctx, id)
}

// TestEffectWorker_TransientSubsGetRetries pins the transient≠definitive
// distinction one seam below NormalizeBoardEvent: a Mongo blink on the
// subscription read must RETRY the row, never MarkDone it (the pre-fix shape
// destroyed the trigger on a one-second store blip).
func TestEffectWorker_TransientSubsGetRetries(t *testing.T) {
	board := &stubConsumingBoard{consumeLeft: 1}
	launcher := &stubEffectLauncher{}
	w, out, ev := effectWorld(t, directConsumeSub(), board, launcher)
	fs := &failingSubs{SubscriptionStore: w.Subs, getErr: errors.New("mongo: server selection timeout")}
	w.Subs = fs

	w.Tick(context.Background(), 10)
	row, _ := out.Row(EffectID(ev.ID, "s1"))
	if row.State != EffectPending || row.Attempts != 1 {
		t.Fatalf("transient Get error: state=%s attempts=%d — want pending/1 (retry), got a destroyed trigger", row.State, row.Attempts)
	}

	// Store recovers → the trigger still fires.
	fs.getErr = nil
	_ = out.MarkRetry(context.Background(), row.ID, row.ClaimID, row.Attempts, time.Now().Add(-time.Second), "")
	w.Tick(context.Background(), 10)
	if launcher.launches != 1 {
		t.Fatalf("launches=%d after recovery, want 1", launcher.launches)
	}
	// A genuinely DELETED subscription stays terminal.
	fs.getErr = ErrSubscriptionNotFound
	_ = out.UpsertPending(context.Background(), []EffectRow{{ID: "e2|s1", TenantID: "t1", SubID: "s1", State: EffectPending}})
	w.Tick(context.Background(), 10)
	if row, _ := out.Row("e2|s1"); row.State != EffectDone {
		t.Fatalf("deleted subscription: state=%s, want done (drop)", row.State)
	}
}

// TestEffectWorker_ReclaimSpendsTheAttemptBudget pins the hung-effect bound:
// an effect that never returns is re-claimed on lease expiry, each reclaim
// spends an attempt, and past the budget the row parks as a dead-letter —
// the pre-fix shape re-ran it every lease forever.
func TestEffectWorker_ReclaimSpendsTheAttemptBudget(t *testing.T) {
	out := NewMemoryEffectOutbox()
	_ = out.UpsertPending(context.Background(), []EffectRow{{ID: "e|s", TenantID: "t1", SubID: "s1", State: EffectPending}})
	now := time.Now().UTC()
	for i := 0; i < MaxEffectAttempts+2; i++ {
		rows, _ := out.ClaimDue(context.Background(), now, 10)
		if len(rows) != 1 {
			t.Fatalf("reclaim %d claimed %d rows", i, len(rows))
		}
		now = now.Add(EffectLease + time.Second) // the worker hangs; lease expires
	}
	row, _ := out.Row("e|s")
	if row.Attempts < MaxEffectAttempts {
		t.Fatalf("attempts=%d after %d reclaims — a hung effect never reaches the budget", row.Attempts, MaxEffectAttempts+2)
	}
	// The worker parks such a row instead of executing it again.
	subs := NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), directConsumeSub())
	launcher := &stubEffectLauncher{}
	final := now.Add(EffectLease + time.Second) // past the last stuck claim's lease
	w := &EffectWorker{Outbox: out, Subs: subs,
		Evaluator: NewEvaluator(subs, WithBoardEffect(&stubConsumingBoard{consumeLeft: 1}), WithLauncher(launcher)),
		Now:       func() time.Time { return final }}
	w.Tick(context.Background(), 10)
	if row, _ := out.Row("e|s"); row.State != EffectFailed {
		t.Fatalf("state=%s after budget exhaustion, want failed (dead-letter)", row.State)
	}
	if launcher.launches != 0 {
		t.Fatal("an exhausted row still launched")
	}
}

// TestEffectOutbox_StaleClaimWritesAreNoOps pins the fence: a worker whose
// lease was stolen cannot resurrect or clobber the new owner's state.
func TestEffectOutbox_StaleClaimWritesAreNoOps(t *testing.T) {
	out := NewMemoryEffectOutbox()
	_ = out.UpsertPending(context.Background(), []EffectRow{{ID: "e|s", TenantID: "t1", SubID: "s1", State: EffectPending}})
	now := time.Now().UTC()
	a, _ := out.ClaimDue(context.Background(), now, 10)                              // worker A
	b, _ := out.ClaimDue(context.Background(), now.Add(EffectLease+time.Second), 10) // B steals the lease
	if len(a) != 1 || len(b) != 1 || a[0].ClaimID == b[0].ClaimID {
		t.Fatalf("claim setup broken: a=%d b=%d", len(a), len(b))
	}
	_ = out.MarkDone(context.Background(), b[0].ID, b[0].ClaimID) // B completes
	// A's late retry write must NOT resurrect the done row.
	_ = out.MarkRetry(context.Background(), a[0].ID, a[0].ClaimID, 1, now, "late loser write")
	if row, _ := out.Row("e|s"); row.State != EffectDone {
		t.Fatalf("state=%s — a stale claim's MarkRetry resurrected a done row", row.State)
	}
}

// TestEffectWorker_EditedSubscriptionReAdmits pins execution-time admission:
// a subscription re-scoped between materialization and execution decides by
// its CURRENT rule.
func TestEffectWorker_EditedSubscriptionReAdmits(t *testing.T) {
	board := &stubConsumingBoard{consumeLeft: 1}
	launcher := &stubEffectLauncher{}
	w, out, ev := effectWorld(t, directConsumeSub(), board, launcher)
	// Re-scope: the rule now requires a label the event does not carry.
	sub, _ := w.Subs.Get(context.Background(), "s1")
	sub.Match.Labels = []string{"security:only"}
	_ = w.Subs.Update(context.Background(), sub)

	w.Tick(context.Background(), 10)
	if launcher.launches != 0 {
		t.Fatal("launched under a stale admission — the edited rule rejects this event")
	}
	if row, _ := out.Row(EffectID(ev.ID, "s1")); row.State != EffectDone {
		t.Fatalf("state=%s, want done (terminal drop)", row.State)
	}
}

// TestEffectWorker_CrossTenantRowIsDropped pins the defence-in-depth guard.
func TestEffectWorker_CrossTenantRowIsDropped(t *testing.T) {
	board := &stubConsumingBoard{consumeLeft: 1}
	launcher := &stubEffectLauncher{}
	subs := NewMemorySubscriptionStore()
	sub := directConsumeSub()
	sub.TenantID = "OTHER"
	_ = subs.Create(context.Background(), sub)
	out := NewMemoryEffectOutbox()
	_ = out.UpsertPending(context.Background(), []EffectRow{{
		ID: "e|s1", TenantID: "t1", SubID: "s1", State: EffectPending,
		Event: Event{ID: "e", Source: SourceBoard, TenantID: "t1", Labels: []string{"triage:auto"}},
	}})
	w := &EffectWorker{Outbox: out, Subs: subs,
		Evaluator: NewEvaluator(subs, WithBoardEffect(board), WithLauncher(launcher))}
	w.Tick(context.Background(), 10)
	if launcher.launches != 0 {
		t.Fatal("a row executed under another tenant's subscription")
	}
	if row, _ := out.Row("e|s1"); row.State != EffectDone {
		t.Fatalf("state=%s, want done (dropped)", row.State)
	}
}

var _ = bundle.ExecutionDirect // keep the import stable across edits
