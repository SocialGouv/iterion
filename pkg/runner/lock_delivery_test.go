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
