package cloudpublisher

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The holds a publication carries (ADR-167 D3) and the launch's re-assertion
// before it publishes (C3), on the filesystem ledger.

// capturedMessages records every message a publisher emitted.
type capturedMessages struct {
	mu   sync.Mutex
	msgs []*queue.RunMessage
}

func (c *capturedMessages) publish(_ context.Context, m *queue.RunMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *capturedMessages) all() []*queue.RunMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*queue.RunMessage(nil), c.msgs...)
}

// reassertHeld reports whether fp's entry of runID is still the one the
// admission `token` wrote.
func reassertHeld(t *testing.T, s store.RunStore, ctx context.Context, fp, runID string, token time.Time) bool {
	t.Helper()
	re, err := store.AsCredSlotStore(s).ReassertCredSlot(ctx, fp, runID, token)
	if err != nil {
		t.Fatalf("ReassertCredSlot %s/%s: %v", fp, runID, err)
	}
	return re.Held
}

// A launch's message names each capped key it reserved by the identity of the
// entry its admission wrote: the runner re-asserts by it. Red when the holds
// are not carried.
func TestCapacityHolds_LaunchMessageCarriesTheAdmissionTokens(t *testing.T) {
	f := newCappedFixture(t, 2)
	p := f.publisher()
	cap := &capturedMessages{}
	p.publishRun = cap.publish
	if err := f.launch(t, p, "run-tokens", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	msgs := cap.all()
	if len(msgs) != 1 || msgs[0].Capacity == nil || len(msgs[0].Capacity.Holds) != 1 {
		t.Fatalf("published %+v, want one message carrying one hold", msgs)
	}
	h := msgs[0].Capacity.Holds[0]
	if h.Subject != cappedFP || h.Token.IsZero() {
		t.Fatalf("hold %+v, want %s with its admission's token", h, cappedFP)
	}
	if !reassertHeld(t, f.spy, f.ctx, cappedFP, "run-tokens", h.Token) {
		t.Fatal("the message's token does not name the entry the launch's admission wrote")
	}
}

// An admission that reserved nothing says so: the message carries an empty
// admission, which a runner tells from a publisher older than the field. Red
// when an empty admission is left off the message.
func TestCapacityHolds_UncappedLaunchCarriesAnEmptyAdmission(t *testing.T) {
	f := newCappedFixture(t, 0)
	p := f.publisher()
	cap := &capturedMessages{}
	p.publishRun = cap.publish
	if err := f.launch(t, p, "run-uncapped", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	msgs := cap.all()
	if len(msgs) != 1 || msgs[0].Capacity == nil || len(msgs[0].Capacity.Holds) != 0 {
		t.Fatalf("published %+v, want one message with an empty, non-nil admission", msgs)
	}
}

// slotLossAtPersist is the stall C3 exists for: between the launch's
// admission and its message, the run's entry is gone (a reconcile on another
// replica evicted it past the grace, the run's document not written yet) —
// and, taker set, another run holds the place. The admission's own instant is
// captured from the reservation the launch makes; the loss lands just before
// the launch persists its run.
type slotLossAtPersist struct {
	store.RunStore
	ledger store.CredSlotStore
	runID  string
	fp     string
	taker  string
	mu     sync.Mutex
	first  time.Time
	again  time.Time
	done   bool
}

func (s *slotLossAtPersist) Unwrap() store.RunStore { return s.RunStore }
func (s *slotLossAtPersist) ReserveCredSlot(ctx context.Context, fp, runID string, max int) (store.CredSlotAdmission, error) {
	adm, err := s.ledger.ReserveCredSlot(ctx, fp, runID, max)
	if err == nil && runID == s.runID && fp == s.fp && adm.Granted {
		s.mu.Lock()
		if s.first.IsZero() {
			s.first = adm.At
		} else {
			s.again = adm.At
		}
		s.mu.Unlock()
	}
	return adm, err
}
func (s *slotLossAtPersist) ReassertCredSlot(ctx context.Context, fp, runID string, token time.Time) (store.CredSlotReassertion, error) {
	return s.ledger.ReassertCredSlot(ctx, fp, runID, token)
}
func (s *slotLossAtPersist) ReleaseCredSlot(ctx context.Context, fp, runID string, at time.Time) error {
	return s.ledger.ReleaseCredSlot(ctx, fp, runID, at)
}
func (s *slotLossAtPersist) SettleCredSlotAdmission(ctx context.Context, runID string, queuedAt time.Time) error {
	return s.ledger.SettleCredSlotAdmission(ctx, runID, queuedAt)
}
func (s *slotLossAtPersist) SaveRun(ctx context.Context, r *store.Run) error {
	s.mu.Lock()
	lose := r.ID == s.runID && !s.done && !s.first.IsZero()
	s.done = s.done || lose
	first := s.first
	s.mu.Unlock()
	if lose {
		// Before the run's document exists: nothing adopts it, and a
		// newcomer takes the place outright.
		if err := s.ledger.ReleaseCredSlot(ctx, s.fp, s.runID, first); err != nil {
			return err
		}
		if s.taker != "" {
			if adm, err := s.ledger.ReserveCredSlot(ctx, s.fp, s.taker, 1); err != nil || !adm.Granted {
				return fmt.Errorf("the taker could not take the place: %+v %v", adm, err)
			}
		}
	}
	return s.RunStore.SaveRun(ctx, r)
}

// C3: a slot lost between the admission and the message is asked for again,
// and the message carries the new reservation's token, not the first's. Red
// when the message is built from the holds as the admission left them.
func TestCapacityHolds_C3ReReservesALostSlotWithItsNewToken(t *testing.T) {
	f := newCappedFixture(t, 1)
	loss := &slotLossAtPersist{RunStore: f.spy, ledger: store.AsCredSlotStore(f.spy), runID: "run-c3", fp: cappedFP}
	p := f.publisher()
	p.store = loss
	cap := &capturedMessages{}
	p.publishRun = cap.publish
	if err := f.launch(t, p, "run-c3", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	msgs := cap.all()
	if len(msgs) != 1 || msgs[0].Capacity == nil || len(msgs[0].Capacity.Holds) != 1 {
		t.Fatalf("published %+v, want one message with one hold", msgs)
	}
	got := msgs[0].Capacity.Holds[0].Token
	if loss.again.IsZero() || !got.Equal(loss.again) || got.Equal(loss.first) {
		t.Fatalf("message token %s, want the re-reservation's %s (the first, lost, was %s)", got, loss.again, loss.first)
	}
}

// C3: a slot lost, and the place taken: the launch is parked with its retry
// armed instead of published, and the slots it still held on its other keys
// are given back. Red when the refusal is returned as an error instead of a
// park, or when a slot re-asserted by C3 is released by its stale instant.
func TestCapacityHolds_C3RefusedParksWithoutPublishing(t *testing.T) {
	f := newCappedFixture(t, 1)
	f.seedLaterCappedKey(t)
	loss := &slotLossAtPersist{RunStore: f.spy, ledger: store.AsCredSlotStore(f.spy), runID: "run-c3-park", fp: cappedFPLater, taker: "run-taker"}
	p := f.publisher()
	p.store = loss
	cap := &capturedMessages{}
	p.publishRun = cap.publish
	sub, err := p.SubmitLaunch(f.ctx, "run-c3-park", launchSpec(nil), cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if sub.Status != store.RunStatusFailedResumable || sub.FailureCode != store.FailureCredSlotWait {
		t.Fatalf("submission %+v, want parked on CRED_SLOT_WAIT", sub)
	}
	if msgs := cap.all(); len(msgs) != 0 {
		t.Fatalf("published %d message(s) for a launch whose place was taken", len(msgs))
	}
	r := f.load(t, "run-c3-park")
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureCredSlotWait || !strings.Contains(r.Error, "openai-capped") {
		t.Fatalf("run: %s/%s %q, want parked on the openai key", r.Status, r.FailureCode, r.Error)
	}
	if _, ok := f.spy.armedFor("run-c3-park"); !ok {
		t.Fatal("parked with no retry armed")
	}
	if n := f.ledgerHolders(t, cappedFP); n != 0 {
		t.Fatalf("the first key names %d holder(s) after the park, want none — C3's re-asserted slot was not given back", n)
	}
}

// A resume's message carries what its own admission reserved — a new entry
// identity — never the launch's. Red when the resume's holds are not its
// admission's.
func TestCapacityHolds_ResumeMessageCarriesItsOwnAdmission(t *testing.T) {
	f := newCappedFixture(t, 2)
	p := f.publisher()
	cap := &capturedMessages{}
	p.publishRun = cap.publish
	if err := f.launch(t, p, "run-resume", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	launchToken := cap.all()[0].Capacity.Holds[0].Token
	if err := f.spy.FailRunResumable(f.ctx, "run-resume", &store.Checkpoint{NodeID: "implement"}, "drained", store.FailureInterrupted); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := f.resume(t, p, "run-resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	msgs := cap.all()
	if len(msgs) != 2 || msgs[1].Resume == nil || msgs[1].Capacity == nil || len(msgs[1].Capacity.Holds) != 1 {
		t.Fatalf("published %+v, want a resume message carrying one hold", msgs)
	}
	resumeToken := msgs[1].Capacity.Holds[0].Token
	if resumeToken.Equal(launchToken) || !reassertHeld(t, f.spy, f.ctx, cappedFP, "run-resume", resumeToken) {
		t.Fatalf("resume token %s (launch %s), want the resume's own admission's entry", resumeToken, launchToken)
	}
	if reassertHeld(t, f.spy, f.ctx, cappedFP, "run-resume", launchToken) {
		t.Fatal("the launch's token still names the entry the resume's admission wrote again")
	}
}
