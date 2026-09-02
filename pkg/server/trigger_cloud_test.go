package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// stubCloudBoardStore drives drainTenant's ordering contract without Mongo:
// it records the ORDER of UpsertPending vs AdvanceTriggerCursor and injects
// per-issue Get faults.
type stubCloudBoardStore struct {
	*trigger.MemoryEffectOutbox
	cursor     int64
	advanceErr error
	// advanceLost simulates a peer replica winning the CAS: the stub's
	// cursor is set to peerCursor and (false, nil) is returned — the
	// boardmongo shape a losing replica sees on every tick.
	advanceLost bool
	peerCursor  int64
	events      []native.Event
	issues      map[string]*native.Issue
	getErr      map[string]error
	log         []string
	upserted    int
	advanced    bool
	advanceTo   int64
}

func (s *stubCloudBoardStore) TriggerCursor() (int64, error) { return s.cursor, nil }

func (s *stubCloudBoardStore) EventsAfter(after int64, _ int) ([]native.Event, error) {
	var out []native.Event
	for _, e := range s.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *stubCloudBoardStore) AdvanceTriggerCursor(from, to int64) (bool, error) {
	if s.advanceErr != nil {
		return false, s.advanceErr
	}
	if s.advanceLost {
		s.cursor = s.peerCursor
		return false, nil
	}
	s.log = append(s.log, "advance")
	s.advanced, s.advanceTo = true, to
	s.cursor = to
	return true, nil
}

func (s *stubCloudBoardStore) Get(id string) (*native.Issue, error) {
	if err := s.getErr[id]; err != nil {
		return nil, err
	}
	if iss, ok := s.issues[id]; ok {
		return iss, nil
	}
	return nil, tracker.ErrNotFound
}

func (s *stubCloudBoardStore) UpsertPending(ctx context.Context, rows []trigger.EffectRow) error {
	s.log = append(s.log, "upsert")
	s.upserted += len(rows)
	return s.MemoryEffectOutbox.UpsertPending(ctx, rows)
}

func newDrainWorld(t *testing.T) (*cloudBoardSource, *stubCloudBoardStore, trigger.SubscriptionStore) {
	t.Helper()
	subs := trigger.NewMemorySubscriptionStore()
	if err := subs.Create(context.Background(), trigger.Subscription{
		ID: "s1", TenantID: "t1", BotID: "triage-bot", Enabled: true,
		Mode:  bundle.ExecutionDirect,
		Match: trigger.Matcher{Sources: []trigger.Source{trigger.SourceBoard}, Labels: []string{"triage:auto"}},
	}); err != nil {
		t.Fatal(err)
	}
	st := &stubCloudBoardStore{
		MemoryEffectOutbox: trigger.NewMemoryEffectOutbox(),
		issues: map[string]*native.Issue{
			"card1": {ID: "card1", Title: "t", State: native.StateInbox, Labels: []string{"triage:auto"}},
			"card2": {ID: "card2", Title: "u", State: native.StateInbox, Labels: []string{"triage:auto"}},
		},
		getErr: map[string]error{},
		events: []native.Event{
			{Type: native.EvtIssueState, IssueID: "card1", Seq: 1, Timestamp: time.Now()},
			{Type: native.EvtIssueState, IssueID: "card2", Seq: 2, Timestamp: time.Now()},
		},
	}
	src := &cloudBoardSource{subs: subs, ctx: context.Background()}
	return src, st, subs
}

// TestDrainTenant_RowsAreDurableBeforeTheCursorMoves pins the ADR-094
// ordering: effects are materialized BEFORE the cursor advances, so no crash
// point loses a matched event.
func TestDrainTenant_RowsAreDurableBeforeTheCursorMoves(t *testing.T) {
	src, st, _ := newDrainWorld(t)
	if _, err := src.drainTenant("t1", st); err != nil {
		t.Fatalf("drainTenant: %v", err)
	}
	if st.upserted != 2 {
		t.Fatalf("upserted %d rows, want 2", st.upserted)
	}
	if len(st.log) < 2 || st.log[0] != "upsert" || st.log[len(st.log)-1] != "advance" {
		t.Fatalf("order = %v — the cursor must advance only after the rows are durable", st.log)
	}
	if st.advanceTo != 2 {
		t.Fatalf("cursor advanced to %d, want 2", st.advanceTo)
	}
}

// TestDrainTenant_TransientReadAbortsBeforeTheCursor pins the F2 fix: a
// transient store error on ANY event of the batch aborts before the cursor
// moves (retry next tick), instead of being treated as a deleted card while
// the cursor sails past it.
func TestDrainTenant_TransientReadAbortsBeforeTheCursor(t *testing.T) {
	src, st, _ := newDrainWorld(t)
	st.getErr["card2"] = errors.New("mongo: server selection timeout")

	_, err := src.drainTenant("t1", st)
	if err == nil {
		t.Fatal("a transient read error must abort the batch, not silently skip the event")
	}
	if st.advanced {
		t.Fatal("cursor advanced past an event that could not be read — that trigger is lost forever")
	}
	if st.upserted != 0 {
		t.Fatalf("partial batch materialized %d rows before aborting", st.upserted)
	}

	// The fault clearing lets the SAME events materialize.
	delete(st.getErr, "card2")
	if materialized, err := src.drainTenant("t1", st); err != nil || !materialized {
		t.Fatalf("retry after fault: materialized=%v err=%v", materialized, err)
	}
	if st.upserted != 2 || !st.advanced {
		t.Fatalf("retry: upserted=%d advanced=%v, want 2/true", st.upserted, st.advanced)
	}
}

// TestDrainTenant_DeletedCardIsADefinitiveSkip: NotFound (the card vanished
// between the transition and the read) skips the event and the cursor still
// advances — that loss is real-world-correct, not a store fault.
func TestDrainTenant_DeletedCardIsADefinitiveSkip(t *testing.T) {
	src, st, _ := newDrainWorld(t)
	delete(st.issues, "card2")

	if _, err := src.drainTenant("t1", st); err != nil {
		t.Fatalf("drainTenant: %v", err)
	}
	if st.upserted != 1 {
		t.Fatalf("upserted %d, want 1 (card1 only)", st.upserted)
	}
	if !st.advanced || st.advanceTo != 2 {
		t.Fatalf("cursor must advance over the deleted card's event: advanced=%v to=%d", st.advanced, st.advanceTo)
	}
}

// TestTrimAtYoungGap pins the contiguous-prefix guard: emit allocates the
// seq BEFORE inserting, so seq N can be visible while N-1 is in flight —
// advancing over that hole loses N-1 forever. A young hole truncates the
// batch; a hole older than the grace is a dead allocation and is stepped
// over.
func TestTrimAtYoungGap(t *testing.T) {
	src := &cloudBoardSource{}
	now := time.Now().UTC()
	events := []native.Event{{Seq: 1}, {Seq: 3}, {Seq: 4}} // hole at 2

	got := src.trimAtYoungGap("t1", 0, events, now)
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("young hole: kept %d events, want just seq 1 (advancing would lose seq 2)", len(got))
	}
	// Still young → still truncated.
	got = src.trimAtYoungGap("t1", 0, events, now.Add(boardTailHoleGrace/2))
	if len(got) != 1 {
		t.Fatalf("hole under the grace: kept %d events, want 1", len(got))
	}
	// Past the grace → dead allocation, stepped over.
	got = src.trimAtYoungGap("t1", 0, events, now.Add(boardTailHoleGrace+time.Second))
	if len(got) != 3 {
		t.Fatalf("expired hole: kept %d events, want all 3", len(got))
	}
	// Contiguous batch untouched, and the watch state cleared.
	got = src.trimAtYoungGap("t1", 0, []native.Event{{Seq: 1}, {Seq: 2}}, now)
	if len(got) != 2 || len(src.holes) != 0 {
		t.Fatalf("contiguous batch: kept %d, holes=%d", len(got), len(src.holes))
	}
}

// TestDrainTenant_TwoPoisonEventsBothClear pins the per-seq poison counter:
// two adjacent unreadable events must not reset each other's count (the
// per-tenant single-slot version froze the tenant forever with an Error
// line every ~60s — the opposite of its contract).
func TestDrainTenant_TwoPoisonEventsBothClear(t *testing.T) {
	src, st, _ := newDrainWorld(t)
	st.getErr["card1"] = errors.New("decode: corrupt doc")
	st.getErr["card2"] = errors.New("decode: corrupt doc")

	for i := 0; i < 3*boardTailPoisonTicks; i++ {
		_, _ = src.drainTenant("t1", st)
		if st.advanced {
			break
		}
	}
	if !st.advanced || st.advanceTo != 2 {
		t.Fatalf("two adjacent poison events froze the tenant: advanced=%v to=%d (threshold=%d, ticks=%d)",
			st.advanced, st.advanceTo, boardTailPoisonTicks, 3*boardTailPoisonTicks)
	}
	if len(src.poisons) != 0 {
		t.Fatalf("poison counters not pruned after the cursor passed them: %d entries", len(src.poisons))
	}
}

// TestDrainTenant_FailedAdvanceKeepsTheAcquiredSkip pins the prune ordering:
// an advance that ERRORS replays the batch next tick — pruning the poison
// counters before it would make an acquired skip cost its 20 ticks again.
func TestDrainTenant_FailedAdvanceKeepsTheAcquiredSkip(t *testing.T) {
	src, st, _ := newDrainWorld(t)
	st.getErr["card1"] = errors.New("decode: corrupt doc")
	st.advanceErr = errors.New("mongo: write concern timeout")

	// Acquire the skip (threshold ticks), with the advance failing.
	for i := 0; i < boardTailPoisonTicks+2; i++ {
		_, _ = src.drainTenant("t1", st)
	}
	key := "t1|1"
	p := src.poisons[key]
	if p == nil || p.fails < boardTailPoisonTicks {
		t.Fatalf("setup: skip not acquired (%+v)", p)
	}
	// One more failed-advance tick must NOT reset the counter.
	_, _ = src.drainTenant("t1", st)
	if src.poisons[key] == nil || src.poisons[key].fails < boardTailPoisonTicks {
		t.Fatal("a failed advance pruned the acquired skip — 20 ticks to re-pay, in a loop")
	}
	// The advance healing prunes it.
	st.advanceErr = nil
	_, _ = src.drainTenant("t1", st)
	if src.poisons[key] != nil {
		t.Fatal("successful advance did not prune the passed counter")
	}
}

// TestDrainTenant_LostCASPrunesOnlyWhatTheStorePassed pins the round-4 HIGH:
// a LOST cursor election — the nominal case on every multi-replica tick —
// must prune poison counters against the cursor the store actually holds
// (the peer may have stopped short of our batch's last), never against a
// seq nobody passed: over-pruning erased an acquired skip and re-froze the
// tenant's tail for another 20 ticks.
func TestDrainTenant_LostCASPrunesOnlyWhatTheStorePassed(t *testing.T) {
	src, st, _ := newDrainWorld(t)
	st.events = append(st.events, native.Event{Type: native.EvtIssueState, IssueID: "card3", Seq: 3, Timestamp: time.Now()})
	st.issues["card3"] = &native.Issue{ID: "card3", Title: "v", State: native.StateInbox, Labels: []string{"triage:auto"}}
	st.getErr["card3"] = errors.New("decode: corrupt doc")

	// Acquire seq-3's skip (advance keeps failing on the CAS: peer stops at 2).
	st.advanceLost, st.peerCursor = true, 2
	for i := 0; i < boardTailPoisonTicks+2; i++ {
		_, _ = src.drainTenant("t1", st)
	}
	p := src.poisons["t1|3"]
	if p == nil || p.fails < boardTailPoisonTicks {
		t.Fatalf("setup: seq-3 skip not acquired (%+v) — the lost CAS pruned it every tick", p)
	}
	// Seqs the peer DID pass are pruned (no leak).
	if src.poisons["t1|1"] != nil || src.poisons["t1|2"] != nil {
		t.Fatal("counters at or below the peer's cursor were not pruned")
	}
	// Winning the election prunes seq 3 once passed.
	st.advanceLost = false
	if _, err := src.drainTenant("t1", st); err != nil {
		t.Fatalf("drain after winning: %v", err)
	}
	if src.poisons["t1|3"] != nil {
		t.Fatal("won advance did not prune the passed counter")
	}
}
