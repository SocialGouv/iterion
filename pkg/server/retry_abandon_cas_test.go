package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The sweeper's half of #2450's first point. The store's conditional write is
// pinned on the twin and against Mongo; what these pin is the SWEEPER: that it
// carries the instant its own scan listed, and that it treats a lost race as
// "this row is not mine" rather than as a settled run.

// TestAbandonRetry_CarriesTheInstantTheScanListed is the wiring half. A
// settlement that passed the zero time would overwrite whatever the row holds
// — including a pull a sibling replica performed one tick ago, whose marker is
// the only durable copy of the provider's announced instant.
func TestAbandonRetry_CarriesTheInstantTheScanListed(t *testing.T) {
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	armed := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)

	s.abandonRetry(context.Background(), st, dueRef("run-settled", armed), "auto-retry abandoned: the bot is gone")

	got, ok := st.abandonExpected["run-settled"]
	if !ok {
		t.Fatal("the settlement never reached the store")
	}
	if !got.Equal(armed) {
		t.Fatalf("the settlement carried %s, want the instant the scan listed (%s): an unconditional write retires a row a sweep tick may have moved",
			instantOrUnknown(got), armed.Format(time.RFC3339))
	}
}

// TestAbandonRetry_ALostRaceLeavesTheRowAlone is the behaviour half: when the
// CAS loses, the row belongs to whoever moved it. Nothing may be counted as
// abandoned and — the part that is not recoverable — the run's outcome must
// not be republished, because every consumer that stood down on the armed
// retry would then treat a run that is still coming back as permanently dead.
func TestAbandonRetry_ALostRaceLeavesTheRowAlone(t *testing.T) {
	st := newFakeRetryStore()
	st.abandonCASLoses = true
	s := newRetrySweeperServer(t, st, nil)
	s.cfg.Metrics = nil

	bus := eventbus.NewInProcBus(nil)
	s.cfg.EventsBus = bus
	replayed := make(chan trigger.Event, 1)
	if _, err := bus.Subscribe("probe", trigger.Matcher{
		Sources: []trigger.Source{trigger.SourceRun},
		Kinds:   []string{trigger.KindRunFailed},
	}, func(_ context.Context, ev trigger.Event) error {
		replayed <- ev
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	armed := time.Now().UTC().Add(-time.Minute)
	s.abandonRetry(context.Background(), st, dueRef("run-raced", armed), "auto-retry abandoned: the bot is gone")

	if st.abandonRefused != 1 {
		t.Fatalf("the store refused %d settlements, want 1 — the fixture is not exercising the lost race", st.abandonRefused)
	}
	if reason, settled := st.abandoned["run-raced"]; settled {
		t.Fatalf("a row the CAS refused was recorded as abandoned (%q)", reason)
	}
	select {
	case ev := <-replayed:
		t.Fatalf("the outcome of a run the settlement did NOT retire was republished (%s): every consumer that stood down on the armed retry now treats a run still waiting on its wall as dead",
			ev.Kind)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestNoticeGate_TheWallThatWasProbedIsNotAnnouncedTwice is #2451's Q3 on the
// pull request. The probe ledger the gate reads belongs to the WALL, so a park
// that re-states the instant already named there is silent and a park on a
// wall the provider MOVED speaks — the case the pull request used to hear only
// at the next charged park.
//
// Named for what it proves, and not "one notice per wall": the evidence is the
// probe ledger, so a wall NO probe ever pulled is announced by each charged
// park on it. The last case below is that bound, asserted rather than assumed.
func TestNoticeGate_TheWallThatWasProbedIsNotAnnouncedTwice(t *testing.T) {
	s, comments, run := newProbeNoticeServer(t)
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)

	// The park that opened the wall: nothing has probed it.
	armed := wall
	run.RetryState = &store.RunRetryState{
		RetryAfter: &armed, Wall: &wall, Reason: store.RetryReasonUsageWindow,
		Code: "USAGE_LIMIT_BLOCKED", Attempts: 1,
	}
	comments.bodies = nil
	s.noticeGatePausedForRetry(context.Background(), run)
	if len(comments.bodies) != 1 {
		t.Fatalf("the park that opened the wall posted %d notices, want 1 — the gating check otherwise sits in flight for the whole wait with no comment", len(comments.bodies))
	}

	// A charged park that re-stated the SAME wall: the probe ledger of that
	// wall survived it, so the pull request hears nothing new.
	probed := now
	run.RetryState = &store.RunRetryState{
		RetryAfter: &armed, Wall: &wall, Reason: store.RetryReasonUsageWindow,
		Code: "USAGE_LIMIT_BLOCKED", Attempts: 2, ProbeCount: 1, LastProbeAt: &probed,
	}
	s.noticeGatePausedForRetry(context.Background(), run)
	if len(comments.bodies) != 1 {
		t.Fatalf("a park that re-stated the wall posted notice %d: one provider window would collect a whole probe budget of byte-identical comments", len(comments.bodies))
	}

	// A wall the provider MOVED: the arming cleared that wall's ledger, and
	// the developer waiting on the check is owed the new instant.
	moved := now.Add(7 * 24 * time.Hour).Truncate(time.Millisecond)
	run.RetryState = &store.RunRetryState{
		RetryAfter: &moved, Wall: &moved, Reason: store.RetryReasonUsageWindow,
		Code: "USAGE_LIMIT_BLOCKED", Attempts: 2,
	}
	s.noticeGatePausedForRetry(context.Background(), run)
	if len(comments.bodies) != 2 {
		t.Fatalf("a wall the provider moved posted %d notices in total, want 2: the row moves and the comment does not, so the pull request names an instant that is no longer the one it waits for", len(comments.bodies))
	}
	if body := comments.bodies[1]; !strings.Contains(body, moved.Format("15:04 UTC on 2006-01-02")) {
		t.Errorf("the second notice does not name the new instant %s:\n%s", moved.Format(time.RFC3339), body)
	}

	// The bound of keying on the probe ledger, stated: a wall NO probe ever
	// pulled (one closer than the probe floor, or any wall under
	// ITERION_RETRY_PROBE_BUDGET=0) keeps an empty ledger for its whole life,
	// so a second charged park on it comments again — up to max_attempts of
	// them, which is what shipped before the wall was persisted. Telling that
	// park from the one that opened the wall needs a second durable field
	// (what the pull request was last told), which this change does not add.
	near := now.Add(20 * time.Minute).Truncate(time.Millisecond)
	for attempt := 1; attempt <= 2; attempt++ {
		run.RetryState = &store.RunRetryState{
			RetryAfter: &near, Wall: &near, Reason: store.RetryReasonUsageWindow,
			Code: "USAGE_LIMIT_BLOCKED", Attempts: attempt,
		}
		s.noticeGatePausedForRetry(context.Background(), run)
	}
	if len(comments.bodies) != 4 {
		t.Errorf("two charged parks on a never-probed wall posted %d notices in total, want 4 (2 + 2): the documented bound is one comment per charged park on such a wall, and a change to it belongs in docs/usage-caps.md too",
			len(comments.bodies))
	}
}

// And the shape the gate must keep answering for: a row from before the wall
// was persisted carries none, so nothing can be told apart per wall. It keeps
// what those rows shipped with — the park that opened the wait speaks, a
// probed re-park stays quiet — rather than going silent on a legacy row for
// ever, which is a required check held in flight with no comment at all.
func TestNoticeGate_ALegacyRowKeepsTheLedgerItShippedWith(t *testing.T) {
	s, comments, run := newProbeNoticeServer(t)
	now := time.Now().UTC()
	armed := now.Add(4 * 24 * time.Hour).Truncate(time.Millisecond)

	comments.bodies = nil
	run.RetryState = &store.RunRetryState{
		RetryAfter: &armed, Reason: store.RetryReasonUsageWindow,
		Code: "USAGE_LIMIT_BLOCKED", Attempts: 1,
	}
	s.noticeGatePausedForRetry(context.Background(), run)
	if len(comments.bodies) != 1 {
		t.Fatalf("a legacy row's park posted %d notices, want 1", len(comments.bodies))
	}
	probed := now
	run.RetryState.ProbeCount, run.RetryState.LastProbeAt = 1, &probed
	s.noticeGatePausedForRetry(context.Background(), run)
	if len(comments.bodies) != 1 {
		t.Fatalf("a probed re-park on a legacy row posted notice %d", len(comments.bodies))
	}
}
