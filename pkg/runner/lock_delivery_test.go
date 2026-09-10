package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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

// hookRecord / hookRecorder capture the LEVEL each line was dispatched to
// the logger's hook at. pkg/log/hook_test.go has the same shape, but it
// lives in package log — this is the handful of lines pkg/runner needs.
type hookRecord struct {
	level iterlog.Level
	msg   string
}

type hookRecorder struct {
	mu   sync.Mutex
	recs []hookRecord
}

func (h *hookRecorder) hook(level iterlog.Level, msg string, _ map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, hookRecord{level: level, msg: msg})
}

func (h *hookRecorder) all() []hookRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookRecord(nil), h.recs...)
}

// The LEVEL is the assertion here, not the message text — do not "simplify"
// this into a substring check, which cannot catch the regression it exists
// for. pkg/log dispatches its hook at warn+, but errtrack.LogHook turns an
// ERROR line into a tracker EVENT (pkg/errtrack/hook.go) and a WARN line
// into a mere breadcrumb that ships only if some later error fires. So a
// lock store that is down — every run in the fleet failing to start, each
// message burning its whole redelivery budget before landing in the DLQ —
// raises an alert only while this path logs at Error. Contention is the
// opposite: a sibling holding the lease is expected traffic on a healthy
// fleet, and paging on it would be noise.
func TestLockFailureLogLevelSeparatesOutageFromContention(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(store.RunStore) store.RunStore
		want  iterlog.Level
	}{
		{"a lock store that cannot answer is an infrastructure failure",
			func(s store.RunStore) store.RunStore { return lockBrokenStore{s} }, iterlog.LevelError},
		{"a sibling holding the lease is ordinary contention",
			func(s store.RunStore) store.RunStore { return lockHeldStore{s} }, iterlog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const id = "lock-level"
			seedRunningRun(t, base, id)
			rec := &hookRecorder{}
			// NOT iterlog.Nop(): its level sits below LevelError, so log()
			// returns before emitting OR dispatching and this test would pass
			// with the bug present.
			logger := iterlog.New(iterlog.LevelInfo, io.Discard)
			logger.SetHook(rec.hook)

			r := &Runner{cfg: Config{Store: tc.store(base), Logger: iterlog.Nop()}, maxDeliverOverride: 3}
			// Deliveries remain, so this takes the DEFERRAL branch: cfg.NATS is
			// nil (the delay falls back to DefaultLockTTL) and NakWithDelay
			// returns nil, so logDeliveryErr is a no-op and the classification
			// line is the only hook record. The archive branch logs lines of
			// its own and would make the count assertion meaningless.
			_, ok, _ := r.acquireRunLock(context.Background(),
				&queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"},
				&fakeDelivery{delivered: 1}, logger)
			if ok {
				t.Fatal("the lock must not be granted when LockRun fails")
			}
			got := rec.all()
			if len(got) != 1 {
				t.Fatalf("want exactly one hook record from the deferral branch, got %+v", got)
			}
			if got[0].level != tc.want {
				t.Fatalf("hook level = %v, want %v (%q)", got[0].level, tc.want, got[0].msg)
			}
		})
	}
}

// wrappedHeldStore returns ErrLockHeld in the shape PRODUCTION delivers it:
// store/mongo's LockRun wraps the provider's error with %w
// (pkg/store/mongo/artifacts.go), so the runner never sees the sentinel bare
// — while every other double here does. That asymmetry is a live trap: swap
// acquireRunLock's errors.Is for an == and the whole suite still passes, but
// the fleet reclassifies ordinary contention as an infrastructure failure
// and starts raising a tracker event per sibling collision.
type wrappedHeldStore struct{ store.RunStore }

func (wrappedHeldStore) LockRun(_ context.Context, runID string) (store.RunLock, error) {
	return nil, fmt.Errorf("store/mongo: acquire lock %s: %w", runID, natsq.ErrLockHeld)
}

func TestWrappedLockHeldIsStillClassifiedAsContention(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "wrapped-held"
	seedRunningRun(t, base, id)
	rec := &hookRecorder{}
	logger := iterlog.New(iterlog.LevelInfo, io.Discard)
	logger.SetHook(rec.hook)

	r := &Runner{cfg: Config{Store: wrappedHeldStore{base}, Logger: iterlog.Nop()}, maxDeliverOverride: 3}
	_, ok, status := r.acquireRunLock(context.Background(),
		&queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"},
		&fakeDelivery{delivered: 1}, logger)

	if ok {
		t.Fatal("the lock must not be granted when a sibling holds it")
	}
	// One classification drives both: the metric label and the log level.
	if status != "lock_held" {
		t.Fatalf("status = %q, want lock_held", status)
	}
	got := rec.all()
	if len(got) != 1 || got[0].level != iterlog.LevelWarn {
		t.Fatalf("wrapped contention must stay a breadcrumb, got %+v", got)
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

// ctxHonouringStore refuses an AppendEvent whose context is already spent.
// FilesystemRunStore DISCARDS ctx (store_events.go), so every other test
// here would pass on an expired one; Mongo threads it into
// guardNotDeleted/allocSeq/InsertOne and fails instantly — and the cloud
// runner, the only place this path executes, uses Mongo. Without this
// double the regression below cannot fail.
type ctxHonouringStore struct{ store.RunStore }

func (s ctxHonouringStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.RunStore.AppendEvent(ctx, runID, evt)
}

func (ctxHonouringStore) LockRun(context.Context, string) (store.RunLock, error) {
	return nil, natsq.ErrLockHeld
}

// A broker outage makes PublishDLQ burn its ENTIRE deadline before giving
// up — it waits for a PubAck. The audit row is then the only confirmed
// trail this delivery leaves (the DLQ copy is, by definition, unconfirmed),
// so it must not be written on the context the publish just spent.
func TestExhaustedPublishDeadlineStillRecordsTheAuditRow(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := seedRunningRun(t, base, "held-publish-timeout")
	r := &Runner{
		cfg:                Config{Store: ctxHonouringStore{base}, Logger: iterlog.Nop()},
		maxDeliverOverride: 3,
		publishTimeout:     time.Millisecond,
		// A broker that never answers: the publish returns only once its own
		// deadline expires, exactly as PublishMsg does awaiting a PubAck.
		lockFailureDLQ: func(ctx context.Context, _ jsDelivery, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	d := &fakeDelivery{delivered: 3}
	r.acquireRunLock(context.Background(),
		&queue.RunMessage{RunID: before.ID, TenantID: "team-1", OwnerID: "u1"}, d, iterlog.Nop())

	events, err := base.LoadEvents(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != store.EventRunDeliveryExhausted {
		t.Fatalf("the exhausted delivery left no trail on the run: %+v", events)
	}
	if events[0].Data["parked"] != false {
		t.Fatalf("an unacknowledged publish is not a confirmed archive: %+v", events[0].Data)
	}
	if got := fmt.Sprint(events[0].Data["error"]); !strings.Contains(got, context.DeadlineExceeded.Error()) {
		t.Fatalf("the row must name why the archive was not confirmed, got %q", got)
	}
	// The invariant the whole path exists for: no lock, no run mutation.
	after := loadStatus(t, base, before.ID)
	if after.Status != before.Status || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("live owner mutated: before=%+v after=%+v", before, after)
	}
	if d.terms != 1 || d.naks != 0 || d.acks != 0 || len(d.nakDelays) != 0 {
		t.Fatalf("delivery transitions = %+v, want exactly one Term", d)
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
