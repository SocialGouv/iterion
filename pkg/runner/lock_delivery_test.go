package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestHeldLockDoesNotSpendFinalDeliverySilently(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "held-final"
	before := seedRunningRun(t, st, id)
	r := &Runner{cfg: Config{Store: lockHeldStore{st}, Logger: iterlog.Nop()}, maxDeliverOverride: 3}
	parks := 0
	r.lockFailureDLQ = func(_ context.Context, _ jsDelivery, reason string) error {
		parks++
		if !strings.Contains(reason, "run state unchanged") {
			t.Fatal(reason)
		}
		return nil
	}
	d := &fakeDelivery{delivered: 3}
	_, ok, _ := r.acquireRunLock(context.Background(), &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}, d, iterlog.Nop())
	if ok || d.naks != 0 || len(d.nakDelays) != 0 {
		t.Fatalf("final delivery was silently nacked away: %+v", d)
	}
	if parks != 1 || d.terms != 1 || d.acks != 0 {
		t.Fatalf("parks=%d delivery=%+v", parks, d)
	}
	after := loadStatus(t, st, id)
	if after.Status != before.Status || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("live owner mutated: before=%+v after=%+v", before, after)
	}
}

// lockBrokenStore answers LockRun with the NON-contention class:
// AcquireLock returns this shape for a KV bucket that is not initialised,
// a marshal failure, or a network blip on the Create — none of which prove
// anything about ownership.
type lockBrokenStore struct{ store.RunStore }

func (lockBrokenStore) LockRun(context.Context, string) (store.RunLock, error) {
	return nil, errors.New("queue/nats: KV create r1: nats: connection closed")
}

// The archived reason is what an admin triages the DLQ entry on, and the
// two lock-failure classes carry different evidence: ErrLockHeld proves a
// live owner, every other error proves only that the lock store did not
// answer. Neither may be described as the other, and the non-contention
// one must never claim the run is free — replaying on top of a live owner
// duplicates the run.
func TestLockFailureReasonSeparatesHeldFromUnconfirmed(t *testing.T) {
	reasonFor := func(t *testing.T, st store.RunStore, id string) string {
		t.Helper()
		var reason string
		r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}, maxDeliverOverride: 3}
		r.lockFailureDLQ = func(_ context.Context, _ jsDelivery, got string) error {
			reason = got
			return nil
		}
		r.acquireRunLock(context.Background(), &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"},
			&fakeDelivery{delivered: 3}, iterlog.Nop())
		return reason
	}
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedRunningRun(t, base, "reason-held")
	seedRunningRun(t, base, "reason-unconfirmed")

	held := reasonFor(t, lockHeldStore{base}, "reason-held")
	unconfirmed := reasonFor(t, lockBrokenStore{base}, "reason-unconfirmed")

	if !strings.Contains(held, "held by another runner") || !strings.Contains(held, "inspect the owner") {
		t.Fatalf("confirmed contention must name the owner: %q", held)
	}
	if strings.Contains(unconfirmed, "held by another runner") {
		t.Fatalf("a lock error that is not contention must not assert an owner: %q", unconfirmed)
	}
	if !strings.Contains(unconfirmed, "could not be confirmed") {
		t.Fatalf("a lock error that is not contention must name ownership as unconfirmed: %q", unconfirmed)
	}
	// Symmetrically: it must not claim the run is FREE either — the lease
	// may well be held by a sibling whose collision never got reported.
	for _, absence := range []string{"no owner", "never claimed", "not held", "nobody"} {
		if strings.Contains(unconfirmed, absence) {
			t.Fatalf("unconfirmed ownership must not be described as absence (%q): %q", absence, unconfirmed)
		}
	}
	// Both stay true about the one thing this path guarantees.
	for _, reason := range []string{held, unconfirmed} {
		if !strings.Contains(reason, "run state unchanged") {
			t.Fatalf("every lock-failure archive leaves the run untouched: %q", reason)
		}
	}
}

func TestHeldLockArchiveFailureLeavesDurableEvidence(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := seedRunningRun(t, st, "held-archive-failure")
	r := &Runner{cfg: Config{Store: lockHeldStore{st}, Logger: iterlog.Nop()}, maxDeliverOverride: 3,
		lockFailureDLQ: func(context.Context, jsDelivery, string) error { return errors.New("broker unavailable") },
	}
	d := &fakeDelivery{delivered: 3}
	r.acquireRunLock(context.Background(), &queue.RunMessage{RunID: before.ID, TenantID: "team-1", OwnerID: "u1"}, d, iterlog.Nop())
	events, err := st.LoadEvents(context.Background(), before.ID)
	if err != nil || len(events) != 1 || events[0].Type != store.EventRunDeliveryExhausted || events[0].Data["parked"] != false || events[0].Data["error"] != "broker unavailable" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	after := loadStatus(t, st, before.ID)
	if after.Status != before.Status || !after.UpdatedAt.Equal(before.UpdatedAt) || d.terms != 1 {
		t.Fatalf("run=%+v delivery=%+v", after, d)
	}
}

// Use a real broker to prove the exhausted delivery is recoverable from the
// DLQ while the run still belongs to its current owner.
func TestHeldLockFinalDeliveryArchivesOnNATS(t *testing.T) {
	conn, _ := schemaRolloutConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := seedRunningRun(t, st, "held-nats")
	wire := &queue.RunMessage{V: queue.SchemaVersion, RunID: before.ID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "wf", IRCompiled: []byte(`{}`), PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano)}
	payload, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, payload); err != nil {
		t.Fatal(err)
	}
	consumer, err := conn.NewConsumer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := consumer.Fetch(ctx, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Nak(); err != nil {
		t.Fatal(err)
	}
	final, err := consumer.Fetch(ctx, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: lockHeldStore{st}, NATS: conn, Logger: iterlog.Nop()}}
	r.acquireRunLock(ctx, wire, final, iterlog.Nop())
	rows, _, err := conn.ListDLQ(ctx, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("DLQ=%+v err=%v", rows, err)
	}
	_, saved, err := conn.PeekDLQ(ctx, rows[0].Seq)
	if err != nil || string(saved) != string(payload) {
		t.Fatalf("archived payload differs: %s err=%v", saved, err)
	}
	after := loadStatus(t, st, before.ID)
	if after.Status != before.Status || after.CASVersion != before.CASVersion {
		t.Fatalf("owner changed: %+v", after)
	}
}
