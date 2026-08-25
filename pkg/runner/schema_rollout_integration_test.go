package runner

// Integration test for issue #481: a mixed-version fleet during a queue
// schema bump must neither burn MaxDeliver in a tight loop nor lose the
// message. It drives the REAL JetStream path — publish a RunMessage whose
// version this build rejects (simulating the other side of a vN → vN+1
// rollout in both directions), consume it with a Runner built on a live
// broker, and walk the full lifecycle: delayed Nak → DLQ park + Term →
// actionable run status → verbatim replay.
//
// Gated on ITERION_TEST_NATS_URI (mirror of ITERION_TEST_MONGO_URI in
// pkg/store/mongo): a plain `go test ./...` skips it, CI's
// `nats-conformance` job provides a `nats -js` service container.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/notify"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// nakDelay is short for wall-clock reasons; production uses
// natsq.SchemaMismatchNakDelay via the Config zero value.
const schemaRolloutTestNakDelay = 500 * time.Millisecond

func schemaRolloutNATSURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_NATS_URI unset — skipping JetStream integration test (CI: nats-conformance job)")
	}
	return uri
}

// schemaRolloutConn wires a Conn with a MaxDeliver of 2 so the exhaustion
// path is one redelivery away, on streams/buckets unique to this test run
// (leftovers from a previous run on a reused broker must not overlap).
func schemaRolloutConn(t *testing.T, uri string) (*natsq.Conn, string) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	stream := "ITERION_RUNS_TEST_" + suffix
	dlq := "ITERION_RUNS_DLQ_TEST_" + suffix
	kv := "test-run-locks-" + suffix
	conn, err := natsq.Connect(context.Background(), natsq.Config{
		URL:          uri,
		StreamName:   stream,
		DLQStream:    dlq,
		KVBucket:     kv,
		ConsumerName: "test-runners-" + suffix,
		MaxDeliver:   2,
		AckWait:      2 * time.Second,
		MaxAge:       time.Hour,
		Logger:       iterlog.Nop(),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.JetStream().DeleteStream(ctx, stream)
		_ = conn.JetStream().DeleteStream(ctx, dlq)
		_ = conn.JetStream().DeleteKeyValue(ctx, kv)
		conn.Close()
	})
	return conn, dlq
}

// publishForeignVersion publishes a RunMessage stamped with a version this
// build does not speak — the on-the-wire shape of the OTHER side of a mixed
// fleet (PublishRun itself would refuse it at Validate, which is the
// producer half of the contract).
func publishForeignVersion(t *testing.T, conn *natsq.Conn, v int, runID, tenantID string, incompatibleShape bool) []byte {
	t.Helper()
	var wire any = &queue.RunMessage{
		V:            v,
		RunID:        runID,
		WorkflowName: "wf-mixed-fleet",
		IRCompiled:   []byte("{}"),
		TenantID:     tenantID,
		OwnerID:      "owner-1",
	}
	if incompatibleShape {
		// A real schema bump may change a field's JSON type. This payload
		// cannot unmarshal into this build's RunMessage at all, so the raw
		// Envelope peek — not Decode's validation error — must still route it
		// through the recoverable mismatch path.
		wire = map[string]any{
			"v": v, "run_id": runID, "workflow_name": "wf-mixed-fleet",
			"ir_compiled": 42, "tenant_id": tenantID, "owner_id": "owner-1",
		}
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, payload, jetstream.WithMsgID(runID)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return payload
}

func TestSchemaRolloutMixedFleet(t *testing.T) {
	uri := schemaRolloutNATSURI(t)

	// Both directions of a strict-equality mismatch: this runner BEHIND a
	// newer producer (server-first rolling upgrade), and this runner AHEAD
	// of messages published before the cutover (the Revi case on #481).
	for _, dir := range []struct {
		name              string
		v                 int
		incompatibleShape bool
	}{
		{"newer producer than consumer, changed wire shape", queue.SchemaVersion + 1, true},
		{"older producer than consumer", queue.SchemaVersion - 1, false},
	} {
		t.Run(dir.name, func(t *testing.T) {
			conn, _ := schemaRolloutConn(t, uri)
			ctx := context.Background()
			runID := fmt.Sprintf("run-mixed-%d-%d", dir.v, time.Now().UnixNano())
			tenantID := "tenant-mixed"
			completionCh := make(chan notify.CompletionPayload, 1)
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				defer req.Body.Close()
				var payload notify.CompletionPayload
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Errorf("decode completion callback: %v", err)
					http.Error(w, "bad payload", http.StatusBadRequest)
					return
				}
				completionCh <- payload
				w.WriteHeader(http.StatusNoContent)
			}))
			defer callback.Close()

			events := eventbus.NewInProcBus(iterlog.Nop())
			eventCh := make(chan trigger.Event, 1)
			cancelEvents, err := events.Subscribe("schema-rollout-test", trigger.Matcher{}, func(_ context.Context, ev trigger.Event) error {
				eventCh <- ev
				return nil
			})
			if err != nil {
				t.Fatalf("subscribe outcome event: %v", err)
			}
			defer cancelEvents()

			// The run document sits `queued`, exactly as SubmitLaunch
			// leaves it before a runner claims the message.
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			now := time.Now().UTC()
			if err := fs.SaveRun(ctx, &store.Run{
				FormatVersion: store.RunFormatVersion,
				ID:            runID,
				WorkflowName:  "wf-mixed-fleet",
				Status:        store.RunStatusQueued,
				CreatedAt:     now,
				UpdatedAt:     now,
				TenantID:      tenantID,
				OwnerID:       "owner-1",
				CallbackURL:   callback.URL,
			}); err != nil {
				t.Fatalf("save run: %v", err)
			}

			payload := publishForeignVersion(t, conn, dir.v, runID, tenantID, dir.incompatibleShape)

			cons, err := conn.NewConsumer(ctx)
			if err != nil {
				t.Fatalf("consumer: %v", err)
			}
			r := &Runner{cfg: Config{
				NATS:                conn,
				Events:              events,
				Store:               fs,
				Logger:              iterlog.Nop(),
				SchemaMismatchDelay: schemaRolloutTestNakDelay,
			}, completionNotifier: notify.New(iterlog.Nop(), time.Second, notify.WithAllowPrivate(true))}

			// --- First delivery: rejected, but NOT in a tight loop. ---
			d1, err := cons.Fetch(ctx, 5*time.Second)
			if err != nil {
				t.Fatalf("first fetch: %v", err)
			}
			if got := d1.NumDelivered(); got != 1 {
				t.Fatalf("first delivery NumDelivered = %d, want 1", got)
			}
			start := time.Now()
			if msg, ok := r.decodeOrTerm(d1); ok || msg != nil {
				t.Fatalf("a foreign-version message must not decode (ok=%v)", ok)
			}

			// An immediate Nak would redeliver within milliseconds; the
			// delayed Nak must leave the queue empty on a short poll…
			if _, err := cons.Fetch(ctx, 150*time.Millisecond); !errors.Is(err, natsq.ErrNoMessage) {
				t.Fatalf("version mismatch redelivered in a tight loop (fetch err = %v)", err)
			}
			// …and hand the message back only after the delay.
			d2, err := cons.Fetch(ctx, 5*time.Second)
			if err != nil {
				t.Fatalf("redelivery fetch: %v", err)
			}
			if elapsed := time.Since(start); elapsed < schemaRolloutTestNakDelay/2 {
				t.Fatalf("redelivery after %v, want ≥ ~%v (delayed Nak)", elapsed, schemaRolloutTestNakDelay)
			}

			// --- Final delivery: DLQ park + Term + actionable status. ---
			if got := d2.NumDelivered(); got != 2 {
				t.Fatalf("second delivery NumDelivered = %d, want 2 (= MaxDeliver)", got)
			}
			if msg, ok := r.decodeOrTerm(d2); ok || msg != nil {
				t.Fatalf("final delivery of a foreign-version message must not decode (ok=%v)", ok)
			}

			// The queue entry is gone (Termed, not dropped by exhaustion).
			if _, err := cons.Fetch(ctx, 300*time.Millisecond); !errors.Is(err, natsq.ErrNoMessage) {
				t.Fatalf("queue entry still present after DLQ park (fetch err = %v)", err)
			}

			// The run document left `queued` with an actionable status.
			run, err := fs.LoadRun(ctx, runID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if run.Status != store.RunStatusFailedResumable {
				t.Fatalf("run status = %q, want %q", run.Status, store.RunStatusFailedResumable)
			}
			if !strings.Contains(run.Error, "schema version") || !strings.Contains(run.Error, "/api/admin/dlq") {
				t.Fatalf("run error %q must name the mismatch AND the replay path", run.Error)
			}
			select {
			case got := <-completionCh:
				if got.RunID != runID || got.Status != string(store.RunStatusFailedResumable) {
					t.Fatalf("completion callback = (run=%q status=%q), want (run=%q status=%q)", got.RunID, got.Status, runID, store.RunStatusFailedResumable)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("schema park fired no completion callback")
			}
			select {
			case got := <-eventCh:
				if got.Kind != trigger.KindRunFailed || got.Subject.ID != runID {
					t.Fatalf("outcome event = (kind=%q run=%q), want (kind=%q run=%q)", got.Kind, got.Subject.ID, trigger.KindRunFailed, runID)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("schema park fired no run-outcome event")
			}

			// The payload is parked VERBATIM, headers explain why.
			parked, _, err := conn.ListDLQ(ctx, 0, 10)
			if err != nil {
				t.Fatalf("list dlq: %v", err)
			}
			if len(parked) != 1 {
				t.Fatalf("DLQ holds %d messages, want 1", len(parked))
			}
			if parked[0].RunID != runID {
				t.Fatalf("DLQ run id = %q, want %q", parked[0].RunID, runID)
			}
			if !strings.Contains(parked[0].Reason, "schema version") {
				t.Fatalf("DLQ reason %q must name the schema mismatch", parked[0].Reason)
			}
			view, raw, err := conn.PeekDLQ(ctx, parked[0].Seq)
			if err != nil {
				t.Fatalf("peek dlq: %v", err)
			}
			if string(raw) != string(payload) {
				t.Fatalf("DLQ payload altered:\n got %s\nwant %s", raw, payload)
			}

			if dir.v > queue.SchemaVersion {
				// Forward recovery: once the fleet speaks the newer schema,
				// replay publishes the exact bytes for that fleet to consume.
				if _, err := conn.RepublishDLQ(ctx, view.Seq); err != nil {
					t.Fatalf("replay: %v", err)
				}
				d3, err := cons.Fetch(ctx, 5*time.Second)
				if err != nil {
					t.Fatalf("fetch after replay: %v", err)
				}
				env, err := d3.Envelope()
				if err != nil {
					t.Fatalf("envelope after replay: %v", err)
				}
				if env.RunID != runID || env.V != dir.v {
					t.Fatalf("replayed envelope = (run=%q v=%d), want (run=%q v=%d)", env.RunID, env.V, runID, dir.v)
				}
				if err := d3.Ack(); err != nil {
					t.Fatalf("ack replayed: %v", err)
				}
			} else {
				// Backward recovery must resume/relaunch at the current schema;
				// replaying these stale bytes would create an endless re-park loop.
				if err := conn.DiscardDLQ(ctx, view.Seq); err != nil {
					t.Fatalf("discard stale backward payload: %v", err)
				}
			}
			if depth, err := conn.DLQDepth(ctx); err != nil || depth != 0 {
				t.Fatalf("DLQ depth after replay = %d (err %v), want 0", depth, err)
			}
		})
	}

	t.Run("DLQ unavailable on final delivery still releases queued run", func(t *testing.T) {
		conn, dlqStream := schemaRolloutConn(t, uri)
		ctx := context.Background()
		runID := fmt.Sprintf("run-mixed-dlq-down-%d", time.Now().UnixNano())
		tenantID := "tenant-mixed"
		fs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		now := time.Now().UTC()
		if err := fs.SaveRun(ctx, &store.Run{
			FormatVersion: store.RunFormatVersion,
			ID:            runID,
			WorkflowName:  "wf-mixed-fleet",
			Status:        store.RunStatusQueued,
			CreatedAt:     now,
			UpdatedAt:     now,
			TenantID:      tenantID,
			OwnerID:       "owner-1",
		}); err != nil {
			t.Fatalf("save run: %v", err)
		}
		publishForeignVersion(t, conn, queue.SchemaVersion+1, runID, tenantID, false)
		cons, err := conn.NewConsumer(ctx)
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
		r := &Runner{cfg: Config{
			NATS:                conn,
			Store:               fs,
			Logger:              iterlog.Nop(),
			SchemaMismatchDelay: schemaRolloutTestNakDelay,
		}}
		d1, err := cons.Fetch(ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("first fetch: %v", err)
		}
		r.decodeOrTerm(d1)
		d2, err := cons.Fetch(ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("final fetch: %v", err)
		}
		if err := conn.JetStream().DeleteStream(ctx, dlqStream); err != nil {
			t.Fatalf("delete DLQ stream to simulate outage: %v", err)
		}
		r.decodeOrTerm(d2)

		run, err := fs.LoadRun(ctx, runID)
		if err != nil {
			t.Fatalf("load run: %v", err)
		}
		if run.Status != store.RunStatusFailedResumable {
			t.Fatalf("run status = %q, want %q despite DLQ outage", run.Status, store.RunStatusFailedResumable)
		}
		if !strings.Contains(run.Error, "DLQ park failed") || !strings.Contains(run.Error, "relaunch") {
			t.Fatalf("run error %q must name the lost DLQ copy and recovery action", run.Error)
		}
		if _, err := cons.Fetch(ctx, 300*time.Millisecond); !errors.Is(err, natsq.ErrNoMessage) {
			t.Fatalf("queue entry still present after exhausted delivery (fetch err = %v)", err)
		}
	})

	t.Run("stale mismatch cannot clobber an active run", func(t *testing.T) {
		conn, _ := schemaRolloutConn(t, uri)
		ctx := context.Background()
		runID := fmt.Sprintf("run-mixed-active-%d", time.Now().UnixNano())
		tenantID := "tenant-mixed"
		fs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		now := time.Now().UTC()
		if err := fs.SaveRun(ctx, &store.Run{
			FormatVersion: store.RunFormatVersion,
			ID:            runID,
			WorkflowName:  "wf-mixed-fleet",
			Status:        store.RunStatusQueued,
			CreatedAt:     now,
			UpdatedAt:     now,
			TenantID:      tenantID,
			OwnerID:       "owner-1",
		}); err != nil {
			t.Fatalf("save run: %v", err)
		}
		publishForeignVersion(t, conn, queue.SchemaVersion-1, runID, tenantID, false)
		cons, err := conn.NewConsumer(ctx)
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}

		events := eventbus.NewInProcBus(iterlog.Nop())
		eventCh := make(chan trigger.Event, 1)
		cancelEvents, err := events.Subscribe("schema-rollout-active-test", trigger.Matcher{}, func(_ context.Context, ev trigger.Event) error {
			eventCh <- ev
			return nil
		})
		if err != nil {
			t.Fatalf("subscribe outcome event: %v", err)
		}
		defer cancelEvents()

		r := &Runner{cfg: Config{
			NATS:                conn,
			Events:              events,
			Store:               fs,
			Logger:              iterlog.Nop(),
			SchemaMismatchDelay: schemaRolloutTestNakDelay,
		}}
		d1, err := cons.Fetch(ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("first fetch: %v", err)
		}
		r.decodeOrTerm(d1)

		// Simulate the documented reverse-direction recovery: the operator
		// resumed the run at the current schema and another pod now owns it,
		// while the stale message is still waiting for its final delivery.
		if err := fs.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
			t.Fatalf("mark active run: %v", err)
		}
		d2, err := cons.Fetch(ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("final stale fetch: %v", err)
		}
		r.decodeOrTerm(d2)

		run, err := fs.LoadRun(ctx, runID)
		if err != nil {
			t.Fatalf("load run: %v", err)
		}
		if run.Status != store.RunStatusRunning {
			t.Fatalf("stale delivery changed active run to %q, want %q", run.Status, store.RunStatusRunning)
		}
		select {
		case ev := <-eventCh:
			t.Fatalf("stale delivery emitted phantom outcome event: kind=%q run=%q", ev.Kind, ev.Subject.ID)
		case <-time.After(150 * time.Millisecond):
		}
	})

	t.Run("resume publication bypasses launch deduplication", func(t *testing.T) {
		conn, _ := schemaRolloutConn(t, uri)
		ctx := context.Background()
		runID := fmt.Sprintf("run-resume-dedup-%d", time.Now().UnixNano())
		base := queue.RunMessage{
			V:              queue.SchemaVersion,
			RunID:          runID,
			WorkflowName:   "wf-resume-dedup",
			IRCompiled:     json.RawMessage(`{}`),
			TenantID:       "tenant-mixed",
			PublishedAtRFC: "2026-08-25T08:00:00Z",
		}
		launchAck, err := conn.PublishRun(ctx, &base)
		if err != nil {
			t.Fatalf("publish launch: %v", err)
		}
		if launchAck.Duplicate {
			t.Fatal("first launch publication was unexpectedly deduplicated")
		}

		resume := base
		resume.Resume = &queue.ResumeSpec{}
		resume.PublishedAtRFC = "2026-08-25T08:01:00Z"
		resumeAck, err := conn.PublishRun(ctx, &resume)
		if err != nil {
			t.Fatalf("publish resume inside dedup window: %v", err)
		}
		if resumeAck.Duplicate {
			t.Fatal("resume was silently swallowed by the launch's Nats-Msg-Id")
		}

		cons, err := conn.NewConsumer(ctx)
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
		for i, wantResume := range []bool{false, true} {
			delivery, err := cons.Fetch(ctx, 5*time.Second)
			if err != nil {
				t.Fatalf("fetch publication %d: %v", i+1, err)
			}
			msg, err := delivery.Decode()
			if err != nil {
				t.Fatalf("decode publication %d: %v", i+1, err)
			}
			if gotResume := msg.Resume != nil; gotResume != wantResume {
				t.Fatalf("publication %d resume=%t, want %t", i+1, gotResume, wantResume)
			}
			if err := delivery.Ack(); err != nil {
				t.Fatalf("ack publication %d: %v", i+1, err)
			}
		}
	})
}
