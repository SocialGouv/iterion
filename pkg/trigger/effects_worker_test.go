package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// stubConsumingBoard is a BoardEffect+LabelConsumer whose consume is a real
// one-shot (spent after the first true), with fault injection on both sides.
type stubConsumingBoard struct {
	promotes    int
	promoteErr  error
	consumes    int
	consumeLeft int // how many consumes still answer true
	consumeErr  error
}

func (f *stubConsumingBoard) Promote(context.Context, LaunchPlan) (string, error) {
	f.promotes++
	return "card", f.promoteErr
}

func (f *stubConsumingBoard) ConsumeMatchLabels(context.Context, string, string, []string) (bool, error) {
	if f.consumeErr != nil {
		return false, f.consumeErr
	}
	f.consumes++
	if f.consumeLeft > 0 {
		f.consumeLeft--
		return true, nil
	}
	return false, nil
}

type stubEffectLauncher struct {
	launches int
	errs     []error // popped per call; empty → nil
}

func (f *stubEffectLauncher) Launch(context.Context, LaunchPlan) (string, error) {
	f.launches++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return "", err
	}
	return "run-1", nil
}

func effectWorld(t *testing.T, sub Subscription, board BoardEffect, l Launcher) (*EffectWorker, *MemoryEffectOutbox, Event) {
	t.Helper()
	subs := NewMemorySubscriptionStore()
	if err := subs.Create(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	eval := NewEvaluator(subs, WithBoardEffect(board), WithLauncher(l))
	out := NewMemoryEffectOutbox()
	ev := Event{
		ID: "board:b:card1:7", Source: SourceBoard, Kind: KindCardMoved,
		TenantID: "t1", Subject: Subject{Type: "card", ID: "card1"},
		Labels: []string{"triage:auto"},
	}
	rows, err := MaterializeEffects(context.Background(), subs, ev, time.Now().UTC())
	if err != nil || len(rows) != 1 {
		t.Fatalf("materialize: rows=%d err=%v", len(rows), err)
	}
	if err := out.UpsertPending(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	return &EffectWorker{Outbox: out, Subs: subs, Evaluator: eval}, out, ev
}

func directConsumeSub() Subscription {
	return Subscription{
		ID: "s1", TenantID: "t1", BotID: "triage-bot", Enabled: true,
		Mode: bundle.ExecutionDirect, ConsumeLabels: true,
		Match: Matcher{Sources: []Source{SourceBoard}, Labels: []string{"triage:auto"}},
	}
}

// TestEffectWorker_LaunchFailureDoesNotLoseTheOneShot pins the F1 core: the
// pre-outbox evaluator consumed the label, then swallowed the launch error —
// the one-shot was spent and the launch never happened. Now the consume is
// marked on the row, the launch failure retries, and the retry launches
// WITHOUT re-consuming.
func TestEffectWorker_LaunchFailureDoesNotLoseTheOneShot(t *testing.T) {
	board := &stubConsumingBoard{consumeLeft: 1}
	launcher := &stubEffectLauncher{errs: []error{errors.New("publisher unavailable")}}
	w, out, ev := effectWorld(t, directConsumeSub(), board, launcher)

	if n := w.Tick(context.Background(), 10); n != 1 {
		t.Fatalf("first tick acted on %d rows, want 1", n)
	}
	row, _ := out.Row(EffectID(ev.ID, "s1"))
	if row.State != EffectPending || !row.ConsumeMarked || row.Attempts != 1 {
		t.Fatalf("after failed launch: state=%s consumeMarked=%v attempts=%d — want pending/marked/1", row.State, row.ConsumeMarked, row.Attempts)
	}

	// Make the row due again and retry: the launch must fire WITHOUT a
	// second consume (the one-shot is already ours).
	_ = out.MarkRetry(context.Background(), row.ID, row.Attempts, time.Now().Add(-time.Second), row.LastError)
	// MarkRetry resets state but must keep ConsumeMarked.
	if r2, _ := out.Row(row.ID); !r2.ConsumeMarked {
		t.Fatal("MarkRetry lost ConsumeMarked — the retry would re-consume or drop the launch")
	}
	if n := w.Tick(context.Background(), 10); n != 1 {
		t.Fatal("retry tick claimed nothing")
	}
	if launcher.launches != 2 {
		t.Fatalf("launches = %d, want 2 (fail then success)", launcher.launches)
	}
	if board.consumes != 1 {
		t.Fatalf("consumes = %d, want exactly 1 — the retry must not spend a second one-shot", board.consumes)
	}
	if row, _ := out.Row(EffectID(ev.ID, "s1")); row.State != EffectDone {
		t.Fatalf("final state = %s, want done", row.State)
	}
}

// TestEffectWorker_SpentOneShotIsTerminal: another event consumed the label
// first — this row completes without launching.
func TestEffectWorker_SpentOneShotIsTerminal(t *testing.T) {
	board := &stubConsumingBoard{consumeLeft: 0} // already spent
	launcher := &stubEffectLauncher{}
	w, out, ev := effectWorld(t, directConsumeSub(), board, launcher)

	w.Tick(context.Background(), 10)
	if launcher.launches != 0 {
		t.Fatal("launched despite a spent one-shot")
	}
	if row, _ := out.Row(EffectID(ev.ID, "s1")); row.State != EffectDone {
		t.Fatalf("state = %s, want done", row.State)
	}
}

// TestEffectWorker_ExhaustionParksVisibly: MaxEffectAttempts real failures
// park the row as failed (a queryable dead-letter), never an infinite loop.
func TestEffectWorker_ExhaustionParksVisibly(t *testing.T) {
	launcher := &stubEffectLauncher{}
	for i := 0; i < MaxEffectAttempts+2; i++ {
		launcher.errs = append(launcher.errs, errors.New("still down"))
	}
	board := &stubConsumingBoard{consumeLeft: 1}
	w, out, ev := effectWorld(t, directConsumeSub(), board, launcher)

	for i := 0; i < MaxEffectAttempts+2; i++ {
		row, _ := out.Row(EffectID(ev.ID, "s1"))
		if row.State == EffectFailed {
			break
		}
		_ = out.MarkRetry(context.Background(), row.ID, row.Attempts, time.Now().Add(-time.Second), "")
		w.Tick(context.Background(), 10)
	}
	row, _ := out.Row(EffectID(ev.ID, "s1"))
	if row.State != EffectFailed {
		t.Fatalf("state = %s after exhaustion, want failed", row.State)
	}
	if launcher.launches > MaxEffectAttempts {
		t.Fatalf("launched %d times, cap is %d", launcher.launches, MaxEffectAttempts)
	}
}

// TestEffectWorker_BoardPromoteRetries: a board-mode promote error retries
// (Promote is idempotent, so at-least-once is safe).
func TestEffectWorker_BoardPromoteRetries(t *testing.T) {
	sub := Subscription{
		ID: "s1", TenantID: "t1", BotID: "handler", Enabled: true,
		Mode:  bundle.ExecutionBoard,
		Match: Matcher{Sources: []Source{SourceBoard}, Labels: []string{"triage:auto"}},
	}
	board := &stubConsumingBoard{promoteErr: errors.New("mongo blink")}
	w, out, ev := effectWorld(t, sub, board, &stubEffectLauncher{})

	w.Tick(context.Background(), 10)
	row, _ := out.Row(EffectID(ev.ID, "s1"))
	if row.State != EffectPending || row.Attempts != 1 {
		t.Fatalf("promote error: state=%s attempts=%d, want pending/1", row.State, row.Attempts)
	}
	board.promoteErr = nil
	_ = out.MarkRetry(context.Background(), row.ID, row.Attempts, time.Now().Add(-time.Second), "")
	w.Tick(context.Background(), 10)
	if row, _ := out.Row(EffectID(ev.ID, "s1")); row.State != EffectDone {
		t.Fatalf("state = %s after retry, want done", row.State)
	}
	if board.promotes != 2 {
		t.Fatalf("promotes = %d, want 2", board.promotes)
	}
}

// TestEffectWorker_ClaimIsExclusive: two ticks cannot claim the same row —
// the second claims nothing while the first's lease holds.
func TestEffectWorker_ClaimIsExclusive(t *testing.T) {
	out := NewMemoryEffectOutbox()
	_ = out.UpsertPending(context.Background(), []EffectRow{{ID: "e|s", TenantID: "t1", State: EffectPending}})
	now := time.Now().UTC()
	a, _ := out.ClaimDue(context.Background(), now, 10)
	b, _ := out.ClaimDue(context.Background(), now, 10)
	if len(a) != 1 || len(b) != 0 {
		t.Fatalf("claims: first=%d second=%d, want 1/0 — a leased row must not be double-claimed", len(a), len(b))
	}
	// The lease expiring makes it reclaimable (orphaned worker recovery).
	c, _ := out.ClaimDue(context.Background(), now.Add(EffectLease+time.Second), 10)
	if len(c) != 1 {
		t.Fatal("expired-lease row was not reclaimable")
	}
}

// TestMaterializeEffects_ObservationalAndDisabled: launched-elsewhere events
// and disabled subscriptions produce no rows.
func TestMaterializeEffects_ObservationalAndDisabled(t *testing.T) {
	subs := NewMemorySubscriptionStore()
	sub := directConsumeSub()
	sub.Enabled = false
	_ = subs.Create(context.Background(), sub)
	ev := Event{ID: "e1", Source: SourceBoard, Kind: KindCardMoved, TenantID: "t1", Labels: []string{"triage:auto"}}
	if rows, _ := MaterializeEffects(context.Background(), subs, ev, time.Now()); len(rows) != 0 {
		t.Fatal("disabled subscription materialized an effect")
	}
	sub.Enabled = true
	_ = subs.Update(context.Background(), sub)
	ev.Payload = map[string]any{PayloadLaunchedRunID: "r1"}
	if rows, _ := MaterializeEffects(context.Background(), subs, ev, time.Now()); len(rows) != 0 {
		t.Fatal("observational (already-launched) event materialized an effect")
	}
}
