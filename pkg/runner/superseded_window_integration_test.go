package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
)

// These drive processOne against a real JetStream (MaxDeliver 2): a delivery
// its run was queued past, while that newer attempt is unclaimed, is
// re-offered — dropped only on its last permitted attempt.

func windowConn(t *testing.T) (*natsq.Conn, *Runner, store.RunStore) {
	t.Helper()
	uri := schemaRolloutNATSURI(t)
	conn, _ := schemaRolloutConn(t, uri)
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := supersededQueuedNakDelay
	supersededQueuedNakDelay = 200 * time.Millisecond
	t.Cleanup(func() { supersededQueuedNakDelay = old })
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	return conn, &Runner{cfg: Config{NATS: conn, Store: fs, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}, fs
}

// windowAttempt is a run parked failed_resumable on its checkpoint after an
// attempt (flip, claim, park), and that attempt's resume message, published.
func windowAttempt(t *testing.T, conn *natsq.Conn, fs store.RunStore) *queue.RunMessage {
	t.Helper()
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	runID := fmt.Sprintf("run-window-%d", time.Now().UnixNano())
	if _, err := fs.CreateRun(ctx, runID, "main", nil); err != nil {
		t.Fatal(err)
	}
	r, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	r.TenantID, r.OwnerID, r.Status, r.Checkpoint = "team-1", "u1", store.RunStatusFailedResumable, &store.Checkpoint{NodeID: "done"}
	if err := fs.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	flip, ok, err := store.AsQueuedFlipper(fs).FlipToQueued(ctx, runID, store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("the attempt's flip: %v %v", ok, err)
	}
	if ok, err := fs.UpdateRunStatusIf(ctx, runID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, err := fs.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, "drain", store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending}, []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
		t.Fatalf("park: %v %v", ok, err)
	}
	pr := parser.Parse("main.bot", "workflow main:\n  entry: done\n")
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: runID, WorkflowName: "main", IRCompiled: body, TenantID: "team-1", OwnerID: "u1",
		PublishedAtRFC: flip.At.Add(time.Millisecond).Format(time.RFC3339Nano),
		Resume:         &queue.ResumeSpec{PriorStatus: store.RunStatusFailedResumable}}
	payload, _ := json.Marshal(msg)
	if _, err := conn.JetStream().Publish(context.Background(), natsq.SubjectRuns, payload, jetstream.WithMsgID(runID+"|resume")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	return msg
}

func fetchOrNil(t *testing.T, cons *natsq.Consumer, wait time.Duration) *natsq.Delivery {
	t.Helper()
	d, err := cons.Fetch(context.Background(), wait)
	if err != nil {
		return nil
	}
	return d
}

// TestProcessOne_aDeliveryReadInsideARefusedFlipsWindowIsReoffered: resume A
// flips the run while the previous attempt's delivery is pending; processOne
// re-offers that delivery instead of acking it, A is refused and reverted,
// and the delivery comes back to run.
func TestProcessOne_aDeliveryReadInsideARefusedFlipsWindowIsReoffered(t *testing.T) {
	conn, r, fs := windowConn(t)
	msg := windowAttempt(t, conn, fs)
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	cons, err := conn.NewConsumer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	flipA, ok, err := store.AsQueuedFlipper(fs).FlipToQueued(ctx, msg.RunID, store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("A's flip: %v %v", ok, err)
	}
	d1 := fetchOrNil(t, cons, 5*time.Second)
	if d1 == nil {
		t.Fatal("no delivery")
	}
	r.processOne(context.Background(), d1)
	if ok, err := store.AsQueuedFlipper(fs).RevertQueuedFlip(ctx, msg.RunID, flipA, "queue resume: refused"); err != nil || !ok {
		t.Fatalf("A's revert: %v %v", ok, err)
	}
	d2 := fetchOrNil(t, cons, 5*time.Second)
	if d2 == nil {
		t.Fatal("the delivery read inside A's window was acked: after A's revert nothing re-offers the run's current attempt")
	}
	if d2.NumDelivered() != 2 {
		t.Fatalf("redelivered as delivery %d, want 2", d2.NumDelivered())
	}
	_ = d2.Ack()
}

// TestProcessOne_aSupersededDeliveryIsDroppedOnItsLastAttempt: re-offered on
// its first attempt, the same delivery is dropped on its last — never parked
// on the DLQ over the newer attempt's run.
func TestProcessOne_aSupersededDeliveryIsDroppedOnItsLastAttempt(t *testing.T) {
	conn, r, fs := windowConn(t)
	msg := windowAttempt(t, conn, fs)
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	cons, err := conn.NewConsumer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.AsQueuedFlipper(fs).FlipToQueued(ctx, msg.RunID, store.RunStatusFailedResumable, time.Now()); err != nil || !ok {
		t.Fatalf("B's flip: %v %v", ok, err)
	}
	before, _ := fs.LoadRun(ctx, msg.RunID)
	for want := 1; want <= 2; want++ {
		d := fetchOrNil(t, cons, 5*time.Second)
		if d == nil || d.NumDelivered() != want {
			t.Fatalf("delivery %d: %v", want, d)
		}
		r.processOne(context.Background(), d)
	}
	if d := fetchOrNil(t, cons, 1500*time.Millisecond); d != nil {
		t.Fatalf("a third delivery (%d) — the last one was not dropped", d.NumDelivered())
	}
	// Dropped, not re-offered into nothing: an acked message leaves the
	// work-queue stream, a Nak'd one past MaxDeliver stays there.
	js := conn.JetStream()
	name, err := js.StreamNameBySubject(context.Background(), natsq.SubjectRuns)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("the last delivery was re-offered into nothing instead of dropped: %d message(s) left in the stream", info.State.Msgs)
	}
	if depth, err := conn.DLQDepth(context.Background()); err != nil || depth != 0 {
		t.Fatalf("DLQ depth %d (%v): a superseded delivery was parked over the newer attempt's run", depth, err)
	}
	after, _ := fs.LoadRun(ctx, msg.RunID)
	if after.Status != before.Status || !after.QueuedAt.Equal(*before.QueuedAt) || after.ContinuationState != before.ContinuationState {
		t.Fatalf("the newer attempt's run moved under a superseded delivery: %s %v %q, was %s %v %q", after.Status, after.QueuedAt, after.ContinuationState, before.Status, before.QueuedAt, before.ContinuationState)
	}
}

// flipAtRunnerBuild queues a newer attempt right after the runner stamps its
// build — under the lock, past the admission's checks, before the engine
// reads the run.
type flipAtRunnerBuild struct {
	store.RunStore
	t    *testing.T
	once sync.Once
}

func (s *flipAtRunnerBuild) Unwrap() store.RunStore { return s.RunStore }

func (s *flipAtRunnerBuild) SetRunnerVersion(ctx context.Context, runID, v string) error {
	err := s.RunStore.SetRunnerVersion(ctx, runID, v)
	s.once.Do(func() {
		if _, ok, ferr := store.AsQueuedFlipper(s.RunStore).FlipToQueued(ctx, runID, store.RunStatusFailedResumable, time.Now()); ferr != nil || !ok {
			s.t.Errorf("the newer attempt's flip: %v %v", ok, ferr)
		}
	})
	return err
}

// TestProcessOne_aResumeTheEngineFoundSupersededIsReoffered: the newer
// attempt lands after the admission, the engine refuses the delivery as
// superseded — and while that attempt is unclaimed the delivery is
// re-offered, with nothing written on the run.
func TestProcessOne_aResumeTheEngineFoundSupersededIsReoffered(t *testing.T) {
	conn, r, fs := windowConn(t)
	msg := windowAttempt(t, conn, fs)
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	r.cfg.Store = &flipAtRunnerBuild{RunStore: fs, t: t}
	cons, err := conn.NewConsumer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d1 := fetchOrNil(t, cons, 5*time.Second)
	if d1 == nil {
		t.Fatal("no delivery")
	}
	r.processOne(context.Background(), d1)
	run, err := fs.LoadRun(ctx, msg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusQueued {
		t.Fatalf("the newer attempt's run is %s after the superseded delivery, want queued — untouched", run.Status)
	}
	d2 := fetchOrNil(t, cons, 5*time.Second)
	if d2 == nil {
		t.Fatal("a resume the engine found superseded while the newer attempt was unclaimed was acked")
	}
	_ = d2.Ack()
}

// flipOnLoad queues a newer attempt right before the nth LoadRun once armed
// — a resume landing between two of the runner's reads of the run. A
// running run is parked first, as a promote would, so the resume can take it.
type flipOnLoad struct {
	store.RunStore
	t     *testing.T
	mu    sync.Mutex
	armed int
	done  bool
}

func (s *flipOnLoad) Unwrap() store.RunStore { return s.RunStore }

func (s *flipOnLoad) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.mu.Lock()
	if s.armed > 0 && !s.done {
		s.armed--
		if s.armed == 0 {
			s.done = true
			r, err := s.RunStore.LoadRun(ctx, id)
			if err != nil {
				s.t.Errorf("load before the newer flip: %v", err)
			} else {
				from := r.Status
				if from == store.RunStatusRunning {
					if ok, err := s.RunStore.UpdateRunOutcome(ctx, id, store.RunStatusFailedResumable, "promoted", store.RunOutcomeMeta{}, []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
						s.t.Errorf("park before the newer flip: %v %v", ok, err)
					}
					from = store.RunStatusFailedResumable
				}
				time.Sleep(2 * time.Millisecond)
				if _, ok, err := store.AsQueuedFlipper(s.RunStore).FlipToQueued(ctx, id, from, time.Now()); err != nil || !ok {
					s.t.Errorf("the newer flip from %s: %v %v", from, ok, err)
				}
			}
		}
	}
	s.mu.Unlock()
	return s.RunStore.LoadRun(ctx, id)
}

func streamMsgs(t *testing.T, conn *natsq.Conn) uint64 {
	t.Helper()
	js := conn.JetStream()
	name, err := js.StreamNameBySubject(context.Background(), natsq.SubjectRuns)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

// TestProcessOne_aDeliveryOutrunPastItsAdmissionIsDroppedOnItsLastAttempt:
// on its last permitted attempt, a delivery a newer attempt outruns between
// its admission and its next read of the run — under its lock, or at the
// adoption of a running doc — is dropped there too, never re-offered into
// nothing.
func TestProcessOne_aDeliveryOutrunPastItsAdmissionIsDroppedOnItsLastAttempt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		nth     int
	}{
		{"under the lock", false, 2},
		{"at the adoption of a running doc", true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, r, fs := windowConn(t)
			r.lockLivenessOverride = true
			msg := windowAttempt(t, conn, fs)
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			if tc.running {
				// An orphan: claimed, its doc old enough to adopt.
				if ok, err := fs.UpdateRunStatusIf(ctx, msg.RunID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusFailedResumable}); err != nil || !ok {
					t.Fatalf("running: %v %v", ok, err)
				}
				run, _ := fs.LoadRun(ctx, msg.RunID)
				run.UpdatedAt = time.Now().UTC().Add(-time.Hour)
				if err := fs.SaveRun(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			cons, err := conn.NewConsumer(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			d1 := fetchOrNil(t, cons, 5*time.Second)
			if d1 == nil {
				t.Fatal("no delivery")
			}
			_ = d1.Nak()
			d2 := fetchOrNil(t, cons, 5*time.Second)
			if d2 == nil || d2.NumDelivered() != 2 {
				t.Fatalf("the last delivery: %v", d2)
			}
			hook := &flipOnLoad{RunStore: fs, t: t, armed: tc.nth}
			r.cfg.Store = hook
			r.processOne(context.Background(), d2)
			if !hook.done {
				t.Fatal("the newer attempt never landed: nothing judged")
			}
			if n := streamMsgs(t, conn); n != 0 {
				t.Fatalf("the last delivery was re-offered into nothing instead of dropped: %d message(s) left in the stream", n)
			}
		})
	}
}
