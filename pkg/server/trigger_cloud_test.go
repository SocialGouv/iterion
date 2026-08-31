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
	cursor    int64
	events    []native.Event
	issues    map[string]*native.Issue
	getErr    map[string]error
	log       []string
	upserted  int
	advanced  bool
	advanceTo int64
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
