package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/nats-io/nats.go/jetstream"
)

// This crosses the real JetStream delivery boundary twice. It intentionally
// drives decode/preconditions/execute, not the entire processOne lease loop.
func TestPreEngineRefusalKeepsAnsweredResumeOnRedelivery(t *testing.T) {
	uri := schemaRolloutNATSURI(t)
	ctx := context.Background()
	conn, _ := schemaRolloutConn(t, uri)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := synctestWorkDir(t)
	r := &Runner{cfg: Config{Store: st, WorkDir: work, Logger: iterlog.Nop()}}
	const id = "pre-engine-redelivery"
	msg := parkingRunMessage(t, id)
	msg.V = queue.SchemaVersion
	msg.TenantID = "tenant-refusal"
	msg.OwnerID = "owner-refusal"
	if err := r.executeRun(ctx, msg, nil, nil); !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("initial pause: %v", err)
	}
	doc, err := st.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	interactionID := doc.Checkpoint.InteractionID
	flip, ok, err := st.FlipToQueued(ctx, id, store.RunStatusPausedWaitingHuman, time.Now())
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	msg.Resume = &queue.ResumeSpec{PriorStatus: store.RunStatusPausedWaitingHuman, Answers: map[string]any{"ok": true}}
	msg.PublishedAtRFC = flip.At.Format(time.RFC3339Nano)
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.JetStream().Publish(ctx, natsq.SubjectRuns, body, jetstream.WithMsgID(id)); err != nil {
		t.Fatal(err)
	}
	consumer, err := conn.NewConsumer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(work, ".mcp.json")
	if err = os.WriteFile(broken, []byte("invalid json"), 0600); err != nil {
		t.Fatal(err)
	}
	d1, err := consumer.Fetch(ctx, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := r.decodeOrTerm(d1)
	if !ok {
		t.Fatal("decode first")
	}
	cause := r.executeRun(ctx, first, nil, nil)
	if cause == nil || !strings.Contains(cause.Error(), "resolve MCP servers") || !isNakAction(classifyExecResult(cause, id).action) {
		t.Fatalf("wrong pre-engine path: %v", cause)
	}
	pending, err := st.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != store.RunStatusQueued || pending.Checkpoint.InteractionID != interactionID || pending.PreEngineRefusal == nil {
		t.Fatalf("PENDING_CONSUMED: %+v", pending)
	}
	if err = d1.Nak(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	d2, err := consumer.Fetch(ctx, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, ok := r.decodeOrTerm(d2)
	if !ok {
		t.Fatal("decode second")
	}
	if pre := r.resolveDeliveryPreconditions(second); !pre.proceed {
		t.Fatalf("redelivery dropped: %s", pre.op)
	}
	cause = r.executeRun(ctx, second, nil, nil)
	doc, err = st.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	interaction, err := st.LoadInteraction(ctx, id, interactionID)
	if err != nil {
		t.Fatal(err)
	}
	if cause != nil || doc.Status != store.RunStatusFinished || interaction.AnsweredAt == nil || doc.PreEngineRefusal != nil {
		t.Fatalf("RESUME_LOST: cause=%v status=%s answered=%v diagnostic=%+v", cause, doc.Status, interaction.AnsweredAt, doc.PreEngineRefusal)
	}
	if d2.NumDelivered() < 2 {
		t.Fatal("fixture did not redeliver")
	}
	if err = d2.Ack(); err != nil {
		t.Fatal(err)
	}
}

func TestPreEngineRefusalDLQPreservesLatestReason(t *testing.T) {
	conn, r, st := windowConn(t)
	msg := windowAttempt(t, conn, st)
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	if e := os.WriteFile(filepath.Join(r.cfg.WorkDir, ".mcp.json"), []byte("invalid json"), 0600); e != nil {
		t.Fatal(e)
	}
	cons, e := conn.NewConsumer(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= 2; i++ {
		d, e := cons.Fetch(ctx, 2*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		if d.NumDelivered() != i {
			t.Fatalf("delivery=%d", d.NumDelivered())
		}
		r.processOne(ctx, d)
	}
	doc, e := st.LoadRun(ctx, msg.RunID)
	if e != nil {
		t.Fatal(e)
	}
	if doc.FailureCode != store.FailureDLQParked {
		t.Fatalf("fixture did not park: status=%s code=%s error=%s", doc.Status, doc.FailureCode, doc.Error)
	}
	latest := doc.Error
	t.Logf("after real processOne DLQ: error=%q diag=%+v", doc.Error, doc.PreEngineRefusal)
	doc.Name = "metadata update after final delivery"
	if e = st.SaveRun(ctx, doc); e != nil {
		t.Fatal(e)
	}
	after, e := st.LoadRun(ctx, msg.RunID)
	if e != nil {
		t.Fatal(e)
	}
	if after.Error != latest || after.PreEngineRefusal != nil {
		t.Fatalf("DLQ_REASON_REPLACED: before=%q after=%q diagnostic=%+v", latest, after.Error, after.PreEngineRefusal)
	}
}
