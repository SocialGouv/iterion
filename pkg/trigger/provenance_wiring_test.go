package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// The provenance CABLE, not the consumers: these tests feed the spine what
// the stores actually PRODUCE (native.StateChangePayload, the migration
// payloads) instead of a hand-built payload — the hand-built form only ever
// certified the consumer, and the whole wire was deletable from production
// with every suite green.

func normalizeFor(t *testing.T, iss *native.Issue, payload map[string]any) Event {
	t.Helper()
	ev, ok, err := NormalizeBoardEvent(
		func(string) (*native.Issue, error) { return iss, nil },
		native.Event{Type: native.EvtIssueState, IssueID: iss.ID, Seq: 1, Timestamp: time.Now().UTC(), Payload: payload},
		"t1", "org/repo", "board")
	if err != nil || !ok {
		t.Fatalf("NormalizeBoardEvent: ok=%v err=%v", ok, err)
	}
	return ev
}

func TestNormalizeBoardEvent_ProvenanceTravelsAndBlanksTheActor(t *testing.T) {
	iss := &native.Issue{ID: "native:c1", Title: "c", State: native.StateReady, Assignee: "jo"}

	// A watchdog's fenced write, as the store really stamps it.
	ev := normalizeFor(t, iss, native.StateChangePayload(native.StateInProgress, native.StateReady, tracker.ReaperMarkerPrefix+"host-a"))
	if got, _ := ev.Payload["reason"].(string); got != tracker.ReasonWatchdog {
		t.Fatalf("reason = %q, want %q — the spine dropped the provenance the store stamped", got, tracker.ReasonWatchdog)
	}
	if ev.Actor != "" {
		t.Fatalf("Actor = %q on a machine repair — the assignee did not act", ev.Actor)
	}

	// An operator's tokenless write: no reason, the assignee is the actor.
	ev = normalizeFor(t, iss, native.StateChangePayload(native.StateInProgress, native.StateReady, ""))
	if _, has := ev.Payload["reason"]; has {
		t.Fatal("an operator move grew a reason")
	}
	if ev.Actor != "jo" {
		t.Fatalf("Actor = %q on an operator move, want the assignee", ev.Actor)
	}

	// A schema migration (reason: state_rename) is machine provenance too:
	// one event per card in the column, none of them an operator gesture.
	ev = normalizeFor(t, iss, map[string]any{"from": "old", "to": "new", "reason": "state_rename"})
	if got, _ := ev.Payload["reason"].(string); got != "state_rename" {
		t.Fatalf("migration reason = %q, dropped", got)
	}
	if ev.Actor != "" {
		t.Fatalf("Actor = %q on a schema migration", ev.Actor)
	}
}

func TestMachineCaused_EnumeratedSet(t *testing.T) {
	for reason, want := range map[string]bool{
		tracker.ReasonWatchdog:    true,
		tracker.ReasonStateRename: true,
		tracker.ReasonStateDelete: true,
		tracker.ReasonFieldRename: true,
		// Descriptive provenance — the cascade of an operator gesture —
		// keeps its triggers. This row is what died under `reason != ""`.
		tracker.ReasonUnblocked: false,
		"":                      false,
	} {
		ev := Event{Payload: map[string]any{}}
		if reason != "" {
			ev.Payload["reason"] = reason
		}
		if got := machineCaused(ev); got != want {
			t.Errorf("machineCaused(reason=%q) = %v, want %v", reason, got, want)
		}
	}
	if machineCaused(Event{}) {
		t.Error("machineCaused on a nil payload")
	}
}

// The OUTBOX half of the benign contract: a machine-caused event must not
// be retried five times and parked as a dead-letter — on cloud the outbox
// is the ONLY delivery path, so every watchdog repair matching a
// consume_labels subscription otherwise manufactures a FAILED row to triage.
func TestEffectWorker_MachineCausedIsTerminalBenign(t *testing.T) {
	subs := NewMemorySubscriptionStore()
	if err := subs.Create(context.Background(), directConsumeSub()); err != nil {
		t.Fatal(err)
	}
	board := &stubConsumingBoard{consumeLeft: 1}
	launcher := &stubEffectLauncher{}
	eval := NewEvaluator(subs, WithBoardEffect(board), WithLauncher(launcher))
	out := NewMemoryEffectOutbox()
	ev := Event{
		ID: "board:b:card1:7", Source: SourceBoard, Kind: KindCardMoved,
		TenantID: "t1", Subject: Subject{Type: "card", ID: "card1"},
		Labels:  []string{"triage:auto"},
		Payload: map[string]any{"reason": tracker.ReasonWatchdog},
	}
	rows, err := MaterializeEffects(context.Background(), subs, ev, time.Now().UTC())
	if err != nil || len(rows) != 1 {
		t.Fatalf("materialize: rows=%d err=%v", len(rows), err)
	}
	if err := out.UpsertPending(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	w := &EffectWorker{Outbox: out, Subs: subs, Evaluator: eval}

	w.Tick(context.Background(), 10)
	row, _ := out.Row(EffectID(ev.ID, "s1"))
	if row.State != EffectDone {
		t.Fatalf("machine-caused effect: state=%s attempts=%d err=%q — want done (benign, the subscription simply does not fire)",
			row.State, row.Attempts, row.LastError)
	}
	if launcher.launches != 0 || board.consumes != 0 {
		t.Fatalf("machine-caused event launched=%d consumed=%d — the one-shot was spent on a repair", launcher.launches, board.consumes)
	}
}

// An UNBLOCKED card is the cascade of an operator closing its blocker —
// intent, not machinery. Deriving machineCaused from the mere presence
// of a reason silently disarmed its one-shot: the card was promoted, the
// label stayed armed, and nothing would ever move it again. Driven from
// the REAL producer (the FS store's promote) through NormalizeBoardEvent
// into applyEffect.
func TestUnblockedCardStillFiresTheOneShot(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	blocker, err := board.Create(native.Issue{Title: "blocker", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	dep, err := board.Create(native.Issue{Title: "dependent", State: native.StateWaitingDeps,
		Blockers: []string{blocker.ID}, Labels: []string{"triage:auto"}, Assignee: "jo"})
	if err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	if _, err := board.SetState(blocker.ID, native.StateDone); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	var promoted *native.Event
	if err := board.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueState && e.IssueID == dep.ID {
			ev := *e
			promoted = &ev
		}
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if promoted == nil {
		t.Fatal("the dependent was never promoted")
	}
	if got, _ := promoted.Payload["reason"].(string); got != tracker.ReasonUnblocked {
		t.Fatalf("promote payload reason = %q, want %q", got, tracker.ReasonUnblocked)
	}

	ev, ok, err := NormalizeBoardEvent(board.Get, *promoted, "t1", "org/repo", "board")
	if err != nil || !ok {
		t.Fatalf("NormalizeBoardEvent: ok=%v err=%v", ok, err)
	}
	if machineCaused(ev) {
		t.Fatal("an unblocked card is machine-caused — its one-shot would never fire and nothing re-arms it")
	}
	if ev.Actor != "jo" {
		t.Fatalf("Actor = %q on an unblocked promote, want the assignee kept", ev.Actor)
	}

	subs := NewMemorySubscriptionStore()
	if err := subs.Create(context.Background(), directConsumeSub()); err != nil {
		t.Fatal(err)
	}
	sb := &stubConsumingBoard{consumeLeft: 1}
	launcher := &stubEffectLauncher{}
	eval := NewEvaluator(subs, WithBoardEffect(sb), WithLauncher(launcher))
	if err := eval.applyEffect(context.Background(), directConsumeSub(), ev, effectOpts{}); err != nil {
		t.Fatalf("applyEffect: %v", err)
	}
	if launcher.launches != 1 || sb.consumes != 1 {
		t.Fatalf("launches=%d consumes=%d — the unblocked card's one-shot must fire exactly once", launcher.launches, sb.consumes)
	}
}
