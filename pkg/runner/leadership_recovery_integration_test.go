package runner

// Integration tests for the leadership-change fast path of the #2890
// self-heal: a NATS election costs seconds, but the Oct 30 wedge showed
// it can freeze the durable consumer's raft state — deliveries stopped
// while pull requests kept waiting, and the loop never recovered. A real
// election cannot be forced against a single-node test broker, so these
// tests exercise the verification's three postures directly against the
// real broker: nothing held (the election was benign), held work moving
// again (the fleet healed on its own — no recreate may fire), and held
// work frozen across the window (the wedge shape — the consumer is
// deleted and recreated, and its work survives the gesture).
// Gated on ITERION_TEST_NATS_URI (CI: nats-conformance job), beside the
// stall self-heal harness.

import (
	"context"
	"testing"
	"time"

	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
)

func consumerCreatedAt(t *testing.T, conn *natsq.Conn, stream, consumer string) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cons, err := conn.JetStream().Consumer(ctx, stream, consumer)
	if err != nil {
		t.Fatalf("consumer handle: %v", err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	return info.Created
}

// A leadership change over an idle consumer is benign: the verification
// reads two postures, sees nothing held, and leaves the consumer alone —
// and it does so well before the window lapses (a healthy election costs
// this pod about a second, not the whole window).
func TestLeadershipRecoveryWithNoWorkLeavesTheConsumerAlone(t *testing.T) {
	conn, stream, consumer := selfHealConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()

	cons, err := conn.PrepareConsumer(ctx)
	if err != nil {
		t.Fatalf("prepare consumer: %v", err)
	}
	createdBefore := consumerCreatedAt(t, conn, stream, consumer)

	start := time.Now()
	outcome := cons.RecoverFromLeadershipChange(ctx, 5*time.Second, false)
	if outcome != natsq.LeadershipRecoveryNoWork {
		t.Fatalf("idle consumer: outcome = %v, want NoWork", outcome)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("idle consumer: the verification ran the whole window (%s) instead of exiting early", elapsed.Round(time.Millisecond))
	}
	if createdAfter := consumerCreatedAt(t, conn, stream, consumer); !createdAfter.Equal(createdBefore) {
		t.Fatalf("idle consumer: the consumer was recreated (created %s → %s) — the fleet pays a recreate per election", createdBefore, createdAfter)
	}
}

// The wedge shape: work sits in the consumer, nothing delivers it, and
// the stream keeps advancing across the whole window (here: one message
// published before the verification, a second published mid-window —
// nobody fetches either; a live fleet would move them, covered by the
// next tests). Both witnesses of the wedge hold — frozen deliveries AND
// inflow — so the verification recreates the consumer, and the held
// messages survive the gesture: both are claimed through the fresh
// consumer afterwards.
func TestLeadershipRecoveryWithFrozenWorkRecreatesTheConsumer(t *testing.T) {
	conn, stream, consumer := selfHealConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()

	cons, err := conn.PrepareConsumer(ctx)
	if err != nil {
		t.Fatalf("prepare consumer: %v", err)
	}
	if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, []byte(`{}`)); err != nil {
		t.Fatalf("publish held work: %v", err)
	}
	createdBefore := consumerCreatedAt(t, conn, stream, consumer)

	// The inflow witness: the stream advances mid-window while nothing
	// delivers — the wedge's second half (Oct 30: stream 12977→12987).
	inflowErr := make(chan error, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		_, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, []byte(`{}`))
		inflowErr <- err
	}()

	outcome := cons.RecoverFromLeadershipChange(ctx, 3*time.Second, false)
	if err := <-inflowErr; err != nil {
		t.Fatalf("publish inflow witness: %v", err)
	}
	if outcome != natsq.LeadershipRecoveryRecreated {
		t.Fatalf("frozen posture with inflow: outcome = %v, want Recreated", outcome)
	}
	if createdAfter := consumerCreatedAt(t, conn, stream, consumer); !createdAfter.After(createdBefore) {
		t.Fatalf("frozen posture: consumer created timestamp did not change (%s → %s) — no recreate happened", createdBefore, createdAfter)
	}

	// The held messages redeliver onto the fresh consumer.
	for i := 0; i < 2; i++ {
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		delivery, err := cons.Fetch(fetchCtx, 5*time.Second)
		cancel()
		if err != nil {
			t.Fatalf("held message %d did not survive the recreate: %v", i, err)
		}
		if err := delivery.Nak(); err != nil {
			t.Fatalf("nak the redelivered message %d: %v", i, err)
		}
	}
}

// A shutdown mid-window decides nothing and claims nothing: the
// verification returns CutShort, the consumer is untouched, and the
// return is prompt even though the wedge shape (held work + inflow) was
// already fully visible — the log arm exists so the operator never reads
// "recreated" for a gesture that did not happen (revi round 2).
func TestLeadershipRecoveryCutShortByShutdownClaimsNoGesture(t *testing.T) {
	conn, stream, consumer := selfHealConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()

	cons, err := conn.PrepareConsumer(ctx)
	if err != nil {
		t.Fatalf("prepare consumer: %v", err)
	}
	if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, []byte(`{}`)); err != nil {
		t.Fatalf("publish held work: %v", err)
	}
	createdBefore := consumerCreatedAt(t, conn, stream, consumer)

	// The inflow witness lands early, so the frozen shape is visible well
	// before the cancellation: without the CutShort arm this scenario
	// answered Recreated without any recreate.
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = conn.JetStream().Publish(ctx, natsq.SubjectRuns, []byte(`{}`))
	}()

	verifyCtx, cancelVerify := context.WithCancel(ctx)
	go func() {
		time.Sleep(900 * time.Millisecond)
		cancelVerify()
	}()

	start := time.Now()
	outcome := cons.RecoverFromLeadershipChange(verifyCtx, 10*time.Second, false)
	if outcome != natsq.LeadershipRecoveryCutShort {
		t.Fatalf("cancelled mid-window: outcome = %v, want CutShort (no gesture may be claimed)", outcome)
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("cancelled mid-window: the verification took %s — the shutdown grace contract erodes", elapsed.Round(time.Millisecond))
	}
	if createdAfter := consumerCreatedAt(t, conn, stream, consumer); !createdAfter.Equal(createdBefore) {
		t.Fatalf("cancelled mid-window: the consumer was recreated (created %s → %s) during shutdown", createdBefore, createdAfter)
	}
}

// The false-positive guard, second half: a healthy consumer that JUST
// delivered (one message fetched and held unacknowledged — a run
// executing) over a quiet stream reads AckPending>0 with a frozen
// delivery sequence, which is the busy fleet's steady state, not the
// wedge. Without the inflow witness this posture paid a recreate on
// every election (revi round 1, finding 1). The window stays under the
// harness's 2s AckWait so no redelivery can move the sequence.
func TestLeadershipRecoveryWithHeldWorkOverAQuietStreamDoesNotRecreate(t *testing.T) {
	conn, stream, consumer := selfHealConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()

	cons, err := conn.PrepareConsumer(ctx)
	if err != nil {
		t.Fatalf("prepare consumer: %v", err)
	}
	if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, []byte(`{}`)); err != nil {
		t.Fatalf("publish work: %v", err)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	delivery, err := cons.Fetch(fetchCtx, 3*time.Second)
	cancel()
	if err != nil {
		t.Fatalf("fetch the executing run's message: %v", err)
	}
	defer func() { _ = delivery.Nak() }()
	createdBefore := consumerCreatedAt(t, conn, stream, consumer)

	outcome := cons.RecoverFromLeadershipChange(ctx, 1200*time.Millisecond, false)
	if outcome != natsq.LeadershipRecoveryNoInflow {
		t.Fatalf("held work over a quiet stream: outcome = %v, want NoInflow (the healthy busy fleet)", outcome)
	}
	if createdAfter := consumerCreatedAt(t, conn, stream, consumer); !createdAfter.Equal(createdBefore) {
		t.Fatalf("held work over a quiet stream: the consumer was recreated (created %s → %s) — a healthy fleet pays a DLQ park per election", createdBefore, createdAfter)
	}
}

// A live fleet heals an election on its own: the held message gets
// delivered mid-window, and the verification must NOT recreate the
// consumer out from under it — the false-positive guard. Without it
// every election would churn the held deliveries of executing runs onto
// the lock-failure DLQ.
func TestLeadershipRecoveryWithMovingWorkDoesNotRecreate(t *testing.T) {
	conn, stream, consumer := selfHealConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()

	cons, err := conn.PrepareConsumer(ctx)
	if err != nil {
		t.Fatalf("prepare consumer: %v", err)
	}
	if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, []byte(`{}`)); err != nil {
		t.Fatalf("publish held work: %v", err)
	}
	createdBefore := consumerCreatedAt(t, conn, stream, consumer)

	// A sibling delivery lands mid-window and is held unacknowledged
	// until the verification concludes — the consumer keeps holding work,
	// but its delivery sequence moved.
	fetched := make(chan *natsq.Delivery, 1)
	fetchErr := make(chan error, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		d, err := cons.Fetch(fetchCtx, 3*time.Second)
		if err != nil {
			fetchErr <- err
			return
		}
		fetched <- d
	}()

	outcome := cons.RecoverFromLeadershipChange(ctx, 5*time.Second, false)
	select {
	case err := <-fetchErr:
		t.Fatalf("sibling fetch: %v", err)
	case d := <-fetched:
		defer func() { _ = d.Nak() }()
	}
	if outcome != natsq.LeadershipRecoveryResumed {
		t.Fatalf("moving posture: outcome = %v, want Resumed", outcome)
	}
	if createdAfter := consumerCreatedAt(t, conn, stream, consumer); !createdAfter.Equal(createdBefore) {
		t.Fatalf("moving posture: the consumer was recreated under a live delivery (created %s → %s)", createdBefore, createdAfter)
	}
}
