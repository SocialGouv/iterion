package trigger

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

type countingNudger struct{ n atomic.Int64 }

func (c *countingNudger) Refresh() { c.n.Add(1) }

func newBoardStore(t *testing.T) *native.Store {
	t.Helper()
	st, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// boardSub is the canonical "label-driven implementer" board trigger: when a
// card enters `ready` carrying `feature`, pin it to feature-dev.
func boardSub() Subscription {
	return Subscription{
		ID:         "board-feature",
		BotID:      "feature-dev",
		Invocation: "board",
		Mode:       "board",
		Enabled:    true,
		ArgsVar:    "feature_prompt",
		Match: Matcher{
			Sources:       []Source{SourceBoard},
			SubjectStates: []string{native.StateReady},
			Labels:        []string{"feature"},
		},
	}
}

// TestPromoteIsIdempotent is the no-double-launch invariant in unit form: the
// board effect only PROMOTES (stamps the bot); the dispatcher Claim is the
// sole launcher. Replaying the same board event N times must converge to ONE
// bot stamp and ONE nudge — so the event fast-path can never cause a second
// dispatch on top of the poll.
func TestPromoteIsIdempotent(t *testing.T) {
	ns := newBoardStore(t)
	iss, err := ns.Create(native.Issue{Title: "Add CSV export", Body: "ship it", State: native.StateReady, Labels: []string{"feature"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	subs := NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), boardSub())
	nudger := &countingNudger{}
	eval := NewEvaluator(subs, WithBoardEffect(NewNativeBoardEffect(ns, nudger, nil)))

	ev := Event{
		Source:  SourceBoard,
		Kind:    KindCardMoved,
		Labels:  []string{"feature"},
		Subject: Subject{Type: "card", ID: iss.ID, Title: iss.Title, Body: iss.Body, State: native.StateReady},
	}

	// Fire the same event three times.
	for i := 0; i < 3; i++ {
		if err := eval.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}

	got, err := ns.Get(iss.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Bot != "feature-dev" {
		t.Fatalf("card bot = %q, want feature-dev", got.Bot)
	}
	if got.BotArgs["feature_prompt"] != "Add CSV export\n\nship it" {
		t.Fatalf("bot args feature_prompt = %q", got.BotArgs["feature_prompt"])
	}
	// Stamped once → nudged once. The 2nd and 3rd identical events are no-ops.
	if n := nudger.n.Load(); n != 1 {
		t.Fatalf("nudger fired %d times, want exactly 1 (idempotent)", n)
	}
}

// TestNonMatchingEventDoesNotPromote guards the matcher: a card in `ready`
// WITHOUT the required label must not be promoted.
func TestNonMatchingEventDoesNotPromote(t *testing.T) {
	ns := newBoardStore(t)
	iss, _ := ns.Create(native.Issue{Title: "chore", State: native.StateReady, Labels: []string{"bug"}})
	subs := NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), boardSub())
	eval := NewEvaluator(subs, WithBoardEffect(NewNativeBoardEffect(ns, nil, nil)))

	_ = eval.Handle(context.Background(), Event{
		Source: SourceBoard, Kind: KindCardMoved, Labels: []string{"bug"},
		Subject: Subject{Type: "card", ID: iss.ID, State: native.StateReady},
	})
	got, _ := ns.Get(iss.ID)
	if got.Bot != "" {
		t.Fatalf("card bot = %q, want empty (no match)", got.Bot)
	}
}

// TestBoardSourceEndToEnd exercises the full live wiring: a real native store
// transition flows through StartBoardSource → InProcBus-equivalent publish →
// evaluator → promote. Uses the source's own bus seam via a channel publisher.
func TestBoardSourceEndToEnd(t *testing.T) {
	ns := newBoardStore(t)
	subs := NewMemorySubscriptionStore()
	_ = subs.Create(context.Background(), boardSub())
	eval := NewEvaluator(subs, WithBoardEffect(NewNativeBoardEffect(ns, nil, nil)))

	// Direct publisher: hand each board event straight to the evaluator
	// (the InProcBus is covered by its own package test; here we prove the
	// board source's normalize + tail path produces a matching event).
	pub := publisherFunc(func(ctx context.Context, ev Event) error { return eval.Handle(ctx, ev) })

	src := StartBoardSource(ns, pub, nil, WithBoardName("default"))
	if src == nil {
		t.Skip("board source unavailable (fsnotify not supported on host)")
	}
	defer src.Stop()

	iss, _ := ns.Create(native.Issue{Title: "Wire export", Body: "csv", State: native.StateInbox, Labels: []string{"feature"}})
	if _, err := ns.SetState(iss.ID, native.StateReady); err != nil {
		t.Fatalf("setstate: %v", err)
	}

	// The tail is async; poll for the promote.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := ns.Get(iss.ID)
		if got != nil && got.Bot == "feature-dev" {
			return // success
		}
		time.Sleep(25 * time.Millisecond)
	}
	got, _ := ns.Get(iss.ID)
	t.Fatalf("card was not promoted via the board source tail; bot=%q", got.Bot)
}

type publisherFunc func(ctx context.Context, ev Event) error

func (f publisherFunc) Publish(ctx context.Context, ev Event) error { return f(ctx, ev) }
