package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestDLQPark_aParkedRunIsResumedNotReplayed, against a real JetStream: a
// resume's last delivery fails and the runner parks it on the DLQ. The run's
// error names the remedy that reaches it — a resume — and not a replay: the
// parked message, admitted against the parked run by the rule the DLQ replay
// checks (queue.Admit), is dropped, so the replay would be refused. The
// operator's resume, a new attempt, is admitted and runs.
func TestDLQPark_aParkedRunIsResumedNotReplayed(t *testing.T) {
	uri := schemaRolloutNATSURI(t)
	conn, _ := schemaRolloutConn(t, uri) // MaxDeliver 2
	ctx := context.Background()
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const tenant, owner = "tenant-1", "owner-1"
	sctx := store.WithIdentity(ctx, tenant, owner)
	runID := fmt.Sprintf("run-dlq-resume-%d", time.Now().UnixNano())
	if err := fs.SaveRun(sctx, &store.Run{ID: runID, TenantID: tenant, OwnerID: owner, Status: store.RunStatusFailedResumable}); err != nil {
		t.Fatal(err)
	}
	publish := func(t *testing.T, from store.RunStatus, msgID string) {
		t.Helper()
		flip, ok, err := store.AsQueuedFlipper(fs).FlipToQueued(sctx, runID, from, time.Now())
		if err != nil || !ok {
			t.Fatalf("the resume's flip from %s: %v %v", from, ok, err)
		}
		wire := &queue.RunMessage{V: queue.SchemaVersion, RunID: runID, WorkflowName: "wf", IRCompiled: json.RawMessage(`{}`),
			TenantID: tenant, OwnerID: owner, PublishedAtRFC: time.Now().UTC().Format(time.RFC3339Nano),
			Resume: &queue.ResumeSpec{PriorStatus: flip.Prior.Status}}
		payload, _ := json.Marshal(wire)
		if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, payload, jetstream.WithMsgID(msgID)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	publish(t, store.RunStatusFailedResumable, runID+"|resume-1")
	cons, err := conn.NewConsumer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{NATS: conn, Store: fs, Logger: iterlog.Nop()}}

	d1, err := cons.Fetch(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("fetch 1: %v", err)
	}
	if _, ok := r.decodeOrTerm(d1); !ok {
		t.Fatal("decode 1")
	}
	_ = d1.Nak()
	d2, err := cons.Fetch(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("fetch 2: %v", err)
	}
	msg2, ok := r.decodeOrTerm(d2)
	if !ok {
		t.Fatal("decode 2")
	}
	if handled, _ := r.parkOnDLQOnFinalDelivery(errors.New("engine: provider 500"), d2, msg2, iterlog.Nop()); !handled {
		t.Fatal("the final delivery was not parked")
	}
	parked, err := fs.LoadRun(sctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.FailureCode != store.FailureDLQParked || !strings.Contains(parked.Error, "resume the run") || strings.Contains(parked.Error, "replay via") {
		t.Fatalf("the parked run: %s/%s %q — want DLQ_PARKED with an error naming the resume, never a replay", parked.Status, parked.FailureCode, parked.Error)
	}
	list, _, err := conn.ListDLQ(ctx, 0, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("dlq list = (%v, %v)", list, err)
	}
	_, payload, err := conn.PeekDLQ(ctx, list[0].Seq)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	var parkedMsg queue.RunMessage
	if err := json.Unmarshal(payload, &parkedMsg); err != nil {
		t.Fatalf("decode the parked payload: %v", err)
	}
	if a := queue.Admit(&parkedMsg, parked); a.Proceeds() || a.Drop != queue.DropDeliberateFailure {
		t.Fatalf("the parked message against the parked run: %+v — a replay of it must be dropped, which is why the DLQ replay refuses it", a)
	}

	// The operator resumes the run: a new attempt, admitted and claimed.
	time.Sleep(2 * time.Millisecond)
	publish(t, store.RunStatusFailedResumable, runID+"|resume-2")
	d3, err := cons.Fetch(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("fetch the resume: %v", err)
	}
	msg3, ok := r.decodeOrTerm(d3)
	if !ok {
		t.Fatal("decode the resume")
	}
	if out := r.resolveDeliveryPreconditions(msg3); !out.proceed {
		t.Fatalf("the operator's resume of a parked run is dropped on admission (%s)", out.op)
	}
	_ = d3.Ack()
}
