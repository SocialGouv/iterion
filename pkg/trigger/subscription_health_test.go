package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A direct launch the spine could not perform — an org launch-gate denial
// above all — must land on the subscription that asked for it. Without it the
// only trace is a warn line in one replica's log: the operator sees a trigger
// that silently stopped firing.

// refusingLauncher refuses every launch with err, and counts the attempts.
type refusingLauncher struct {
	err      error
	launches int
}

func (l *refusingLauncher) Launch(context.Context, LaunchPlan) (string, error) {
	l.launches++
	return "run-1", l.err
}

func directSub(id string) Subscription {
	return Subscription{
		ID:         id,
		BotID:      "probe",
		Invocation: bundle.InvocationKindBoard,
		Mode:       bundle.ExecutionDirect,
		Match:      Matcher{Sources: []Source{SourceCustom}},
		Enabled:    true,
		CreatedAt:  time.Now().UTC(),
	}
}

func customEvent() Event {
	return Event{ID: "custom:probe:1", Source: SourceCustom, Kind: "probe", OccurredAt: time.Now().UTC()}
}

func TestEvaluator_LaunchRefusalLandsOnTheSubscription(t *testing.T) {
	ctx := context.Background()
	subs := NewMemorySubscriptionStore()
	if err := subs.Create(ctx, directSub("sub-1")); err != nil {
		t.Fatal(err)
	}
	l := &refusingLauncher{err: errors.New("launch gate: concurrency_cap_exceeded: org has 3 active runs (cap 3)")}
	eval := NewEvaluator(subs, WithLauncher(l))

	if err := eval.Handle(ctx, customEvent()); err != nil {
		t.Fatalf("Handle = %v, want nil (one failing subscription must not fail the event)", err)
	}
	got, err := subs.Get(ctx, "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError == "" {
		t.Fatal("the refused launch left no last_error on the subscription — the operator has nothing to read but a pod log")
	}
	if got.LastError != l.err.Error() {
		t.Errorf("last_error = %q, want the refusal verbatim %q", got.LastError, l.err.Error())
	}
	if got.LastErrorAt == nil {
		t.Error("last_error carries no instant — an operator cannot tell a refusal from last month from one a minute ago")
	}
}

func TestEvaluator_ALaunchThatGoesThroughClearsTheFlag(t *testing.T) {
	ctx := context.Background()
	subs := NewMemorySubscriptionStore()
	sub := directSub("sub-1")
	sub.LastError = "launch gate: monthly_run_quota_exceeded: monthly run quota (10) exhausted"
	when := time.Now().UTC().Add(-time.Hour)
	sub.LastErrorAt = &when
	if err := subs.Create(ctx, sub); err != nil {
		t.Fatal(err)
	}
	eval := NewEvaluator(subs, WithLauncher(&refusingLauncher{}))

	if err := eval.Handle(ctx, customEvent()); err != nil {
		t.Fatalf("Handle = %v, want nil", err)
	}
	got, err := subs.Get(ctx, "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError != "" || got.LastErrorAt != nil {
		t.Errorf("a launch that went through left last_error=%q at=%v — a stale refusal reads as a live one", got.LastError, got.LastErrorAt)
	}
}

// A benign non-fire is not a refusal: a machine-caused event owes no effect,
// so it must not stamp a failure on a healthy subscription.
func TestEvaluator_MachineCausedEventLeavesTheHealthAlone(t *testing.T) {
	ctx := context.Background()
	subs := NewMemorySubscriptionStore()
	if err := subs.Create(ctx, directSub("sub-1")); err != nil {
		t.Fatal(err)
	}
	l := &refusingLauncher{err: errors.New("should never be called")}
	eval := NewEvaluator(subs, WithLauncher(l))

	ev := customEvent()
	ev.Payload = map[string]any{"reason": "watchdog"}
	if err := eval.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle = %v, want nil", err)
	}
	if l.launches != 0 {
		t.Fatalf("a machine-caused event launched %d time(s), want 0", l.launches)
	}
	got, _ := subs.Get(ctx, "sub-1")
	if got.LastError != "" {
		t.Errorf("last_error = %q after a benign non-fire, want empty", got.LastError)
	}
}

// The health write is targeted, so an operator edit made between the match and
// the record survives it (both twins; the Mongo half is a $set on the two
// fields).
func TestMemorySubscriptionStore_MarkLaunchErrorDoesNotClobberAnEdit(t *testing.T) {
	ctx := context.Background()
	subs := NewMemorySubscriptionStore()
	stale := directSub("sub-1")
	if err := subs.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	// The operator re-points the subscription at another bot after the
	// evaluator read its copy.
	edited := stale
	edited.BotID = "other"
	if err := subs.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if err := subs.MarkLaunchError(ctx, "sub-1", "boom", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := subs.Get(ctx, "sub-1")
	if got.BotID != "other" {
		t.Errorf("bot_id = %q, want %q — the health write replaced the row instead of setting its two fields", got.BotID, "other")
	}
	if got.LastError != "boom" {
		t.Errorf("last_error = %q, want %q", got.LastError, "boom")
	}
	if err := subs.MarkLaunchError(ctx, "ghost", "boom", time.Now()); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("MarkLaunchError on an unknown id = %v, want ErrSubscriptionNotFound", err)
	}
}
