package trigger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

type failDurableConsumedOnce struct {
	EffectOutbox
	failed bool
}

func (o *failDurableConsumedOnce) MarkConsumed(ctx context.Context, id, claim string) error {
	if !o.failed {
		o.failed = true
		return errors.New("injected consume-marker failure")
	}
	return o.EffectOutbox.MarkConsumed(ctx, id, claim)
}

// Neither the worker, the card store nor the outbox retains process memory
// across the failed marker write. Only the atomically persisted receipt can
// distinguish this effect's consumption from a competing one-shot.
func TestRound3ConsumedMarkerFailureSurvivesRestartAndHold(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	openBoard := func() *native.Store {
		st, err := native.NewStore(filepath.Join(root, "board"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	board := openBoard()
	card, err := board.Create(native.Issue{Title: "one-shot", State: native.StateInbox, Labels: []string{"triage:auto", "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	subs := seed(t, directConsumeSub())
	ev := Event{ID: "board:once", TenantID: "t1", Source: SourceBoard, Kind: KindCardMoved,
		Subject: Subject{Type: "card", ID: card.ID}, Labels: []string{"triage:auto"}}
	now := time.Now().UTC()
	rows, err := MaterializeEffects(ctx, subs, nil, ev, now)
	if err != nil || len(rows) != 1 {
		t.Fatalf("materialize: %v %v", rows, err)
	}
	path := filepath.Join(root, "effects.json")
	out := NewFileEffectOutbox(path)
	if err := out.UpsertPending(ctx, rows); err != nil {
		t.Fatal(err)
	}
	l := &heldLauncher{hold: true}
	fault := &failDurableConsumedOnce{EffectOutbox: out}
	w := EffectWorker{Outbox: fault, Subs: subs, Now: func() time.Time { return now },
		Evaluator: NewEvaluator(subs, WithLauncher(l), WithBoardEffect(NewNativeBoardEffect(board, nil, nil)))}
	if n := w.Tick(ctx, 10); n != 1 || !fault.failed {
		t.Fatalf("injected tick: claimed=%d failed=%v", n, fault.failed)
	}
	if err := board.Close(); err != nil {
		t.Fatal(err)
	}
	board = openBoard()
	card, err = board.Get(card.ID)
	if err != nil || len(card.Labels) != 1 || card.Labels[0] != "keep" || len(card.ConsumedEffects) != 1 || card.ConsumedEffects[0] != rows[0].ID {
		t.Fatalf("labels and receipt must survive together: %+v %v", card, err)
	}
	w.Outbox = NewFileEffectOutbox(path)
	w.Evaluator = NewEvaluator(subs, WithLauncher(l), WithBoardEffect(NewNativeBoardEffect(board, nil, nil)))
	for i := 0; i < MaxEffectAttempts+2; i++ {
		now = now.Add(time.Hour)
		if n := w.Tick(ctx, 10); n != 1 {
			t.Fatalf("maintenance cycle %d retired the recoverable effect: claimed=%d", i, n)
		}
	}
	if len(l.plans) != 0 {
		t.Fatal("launched under maintenance")
	}
	l.hold = false
	now = now.Add(time.Hour)
	w.Tick(ctx, 10)
	now = now.Add(time.Hour)
	if n := w.Tick(ctx, 10); n != 0 || len(l.plans) != 1 || l.plans[0].Event.ID != ev.ID {
		t.Fatalf("after disarm: claimed=%d plans=%+v", n, l.plans)
	}
}
