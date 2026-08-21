package runstream

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The suite needs a real replica set (change streams) — same gate as
// pkg/store/mongo/conformance_test.go. docker-compose.cloud.yml's
// mongo service initiates rs0; from the host use
// ITERION_TEST_MONGO_URI='mongodb://localhost:27017/?directConnection=true'.
func newMongoFixture(t *testing.T) (*mongostore.Store, *MongoSource, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set — skipping Mongo runstream tests")
	}
	dbName := fmt.Sprintf("iterion_runstream_%d", time.Now().UnixNano())
	st, err := mongostore.New(context.Background(), mongostore.Config{URI: uri, Database: dbName, Blob: nopBlob{}})
	if err != nil {
		t.Fatalf("mongostore.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = st.DB().Drop(ctx)
		_ = st.Close(ctx)
	})
	src := NewMongo(st.EventsCollection(), st.RunLogsCollection(), st.RunsCollection(), quietLogger())
	ctx := store.WithIdentity(context.Background(), "_test", "_test")
	return st, src, ctx
}

func appendMongoEvent(t *testing.T, ctx context.Context, st *mongostore.Store, runID, nodeID string) {
	t.Helper()
	if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventNodeStarted, NodeID: nodeID}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// TestMongoSource_EventsBackfillThenTail: the basic replay + live-tail
// lifecycle over a real change stream.
func TestMongoSource_EventsBackfillThenTail(t *testing.T) {
	st, src, ctx := newMongoFixture(t)
	const runID = "m-events"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for i := 0; i < 3; i++ {
		appendMongoEvent(t, ctx, st, runID, "seeded")
	}

	sub, err := src.SubscribeEvents(ctx, runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	var got []*store.Event
	deadline := time.After(15 * time.Second)
	for len(got) < 3 {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatalf("stream closed during backfill; got %d", len(got))
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatalf("timeout on backfill; got %d", len(got))
		}
	}

	appendMongoEvent(t, ctx, st, runID, "live")
	deadline = time.After(15 * time.Second)
	for len(got) < 4 {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatal("stream closed before the live event")
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatal("timeout on live tail")
		}
	}
	if got[3].NodeID != "live" || got[3].Seq != 3 {
		t.Fatalf("live event = %+v, want NodeID live seq 3", got[3])
	}
	// No duplicates across the backfill/tail boundary.
	seen := map[int64]int{}
	for _, ev := range got {
		seen[ev.Seq]++
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("seq %d delivered %d times", seq, n)
		}
	}
}

// TestMongoSource_EventsNoBoundaryGap is the regression test for the
// replay→watch blind window: events inserted concurrently with
// Subscribe (racing the backfill/tail boundary) must all be delivered.
// The pre-fix ordering (replay THEN watch) lost any insert landing
// between the two phases.
func TestMongoSource_EventsNoBoundaryGap(t *testing.T) {
	st, src, ctx := newMongoFixture(t)
	const runID = "m-gap"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	appendMongoEvent(t, ctx, st, runID, "seed")

	// Hammer inserts while subscribing so some land in the historical
	// window and some race the boundary.
	const total = 40
	insertDone := make(chan error, 1)
	go func() {
		for i := 1; i < total; i++ {
			if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventNodeStarted, NodeID: "racer"}); err != nil {
				insertDone <- err
				return
			}
		}
		insertDone <- nil
	}()

	sub, err := src.SubscribeEvents(ctx, runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	seen := map[int64]bool{}
	deadline := time.After(30 * time.Second)
	for len(seen) < total {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatalf("stream closed; delivered %d/%d", len(seen), total)
			}
			for _, ev := range batch {
				if seen[ev.Seq] {
					t.Fatalf("seq %d delivered twice", ev.Seq)
				}
				seen[ev.Seq] = true
			}
		case <-deadline:
			t.Fatalf("boundary gap: delivered %d/%d events", len(seen), total)
		}
	}
	if err := <-insertDone; err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}
}

// TestMongoSource_EventsReplayTailTerminal: backfill, live change-stream
// tail, and stream close (after a final backfill) when the run flips
// terminal — the event twin of TestMongoSource_LogsReplayTailTerminal.
// Before the fix the event change stream had no terminal poll, so it
// pumped forever and the WS never emitted `terminated`.
func TestMongoSource_EventsReplayTailTerminal(t *testing.T) {
	prev := mongoTerminalPollInterval
	mongoTerminalPollInterval = 150 * time.Millisecond
	t.Cleanup(func() { mongoTerminalPollInterval = prev })

	st, src, ctx := newMongoFixture(t)
	const runID = "m-events-terminal"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	appendMongoEvent(t, ctx, st, runID, "seeded")

	sub, err := src.SubscribeEvents(ctx, runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	// Backfill delivers the seeded event.
	drainMongoEvents(t, sub, 1, 15*time.Second)

	// A live event through the change stream.
	appendMongoEvent(t, ctx, st, runID, "live")
	drainMongoEvents(t, sub, 1, 15*time.Second)

	// Persist a final event, then flip terminal → final backfill (the
	// racing event included) → close.
	appendMongoEvent(t, ctx, st, runID, "final")
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	// The stream must deliver the final event (if not already tailed) and
	// then close within a few poll ticks.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-sub.Events():
			if !ok {
				return // channel closed → WS emits terminated
			}
		case <-deadline:
			t.Fatal("timeout: event stream did not close after terminal flip")
		}
	}
}

// drainMongoEvents reads at least n events (across batches) or fails.
func drainMongoEvents(t *testing.T, sub EventSubscription, n int, timeout time.Duration) {
	t.Helper()
	got := 0
	deadline := time.After(timeout)
	for got < n {
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				t.Fatalf("stream closed early after %d events, want %d", got, n)
			}
			got += len(batch)
		case <-deadline:
			t.Fatalf("timeout after %d events, want %d", got, n)
		}
	}
}

// TestMongoSource_LogsReplayTailTerminal: mid-chunk from_offset
// backfill, live change-stream tail, and stream close (after a final
// drain) when the run flips terminal.
func TestMongoSource_LogsReplayTailTerminal(t *testing.T) {
	prev := mongoTerminalPollInterval
	mongoTerminalPollInterval = 150 * time.Millisecond
	t.Cleanup(func() { mongoTerminalPollInterval = prev })

	st, src, ctx := newMongoFixture(t)
	const runID = "m-logs"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	ls := store.AsRunLogStore(st)
	if err := ls.AppendRunLog(ctx, runID, 0, []byte("0123456789")); err != nil {
		t.Fatalf("AppendRunLog: %v", err)
	}

	// from_offset falls inside the persisted chunk → sliced backfill.
	sub, err := src.SubscribeLogs(ctx, runID, 4)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	defer sub.Close()

	buf := []byte("0123") // client-held prefix
	collect := func(want string, timeout time.Duration) {
		t.Helper()
		deadline := time.After(timeout)
		for string(buf) != want {
			select {
			case chunk, ok := <-sub.Chunks():
				if !ok {
					t.Fatalf("stream closed early; have %q want %q", buf, want)
				}
				if chunk.Offset > int64(len(buf)) {
					t.Fatalf("gap: have %d bytes, chunk at %d", len(buf), chunk.Offset)
				}
				if end := chunk.Offset + int64(len(chunk.Data)); end > int64(len(buf)) {
					buf = append(buf[:chunk.Offset], chunk.Data...)
				}
			case <-deadline:
				t.Fatalf("timeout; have %q want %q", buf, want)
			}
		}
	}
	collect("0123456789", 15*time.Second)

	// Live chunk through the change stream.
	if err := ls.AppendRunLog(ctx, runID, 10, []byte("ABC")); err != nil {
		t.Fatalf("AppendRunLog live: %v", err)
	}
	collect("0123456789ABC", 15*time.Second)

	// Terminal flip → final drain (a chunk racing the flip included) →
	// close.
	if err := ls.AppendRunLog(ctx, runID, 13, []byte("tail")); err != nil {
		t.Fatalf("AppendRunLog tail: %v", err)
	}
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case chunk, ok := <-sub.Chunks():
			if !ok {
				if got := string(buf); got != "0123456789ABCtail" {
					t.Fatalf("final stream = %q, want %q", got, "0123456789ABCtail")
				}
				return
			}
			if end := chunk.Offset + int64(len(chunk.Data)); end > int64(len(buf)) && chunk.Offset <= int64(len(buf)) {
				buf = append(buf[:chunk.Offset], chunk.Data...)
			}
		case <-deadline:
			t.Fatal("timeout waiting for terminal close")
		}
	}
}

// TestMongoSource_TenantScopingIsolatesStreams: a subscriber whose ctx
// carries tenant A must not receive tenant B's events for a same-named
// run (the change-stream and backfill filters are tenant-scoped).
func TestMongoSource_TenantScopingIsolatesStreams(t *testing.T) {
	st, src, _ := newMongoFixture(t)
	const runID = "m-tenant"
	ctxA := store.WithIdentity(context.Background(), "tenant-a", "u")
	ctxB := store.WithIdentity(context.Background(), "tenant-b", "u")
	if _, err := st.CreateRun(ctxB, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	appendMongoEvent(t, ctxB, st, runID, "b-only")

	sub, err := src.SubscribeEvents(ctxA, runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	// Live insert for tenant B while A listens.
	appendMongoEvent(t, ctxB, st, runID, "b-live")

	select {
	case batch, ok := <-sub.Events():
		if ok && len(batch) > 0 {
			t.Fatalf("tenant A received tenant B's events: %+v", batch)
		}
	case <-time.After(10 * time.Second):
		// Quiet — correct.
	}
}

// nopBlob satisfies the store's required blob.Client dependency; these
// tests never touch artifacts or attachments.
type nopBlob struct{}

func (nopBlob) PutArtifact(context.Context, string, string, int, []byte) error { return nil }
func (nopBlob) GetArtifact(context.Context, string, string, int) ([]byte, error) {
	return nil, fmt.Errorf("nopBlob: not found")
}
func (nopBlob) ListArtifactVersions(context.Context, string, string) ([]int, error) {
	return nil, nil
}
func (nopBlob) DeleteRun(context.Context, string) error { return nil }
func (nopBlob) Ping(context.Context) error              { return nil }
func (nopBlob) Close() error                            { return nil }
func (nopBlob) PutAttachment(context.Context, string, string, string, string, []byte) error {
	return nil
}
func (nopBlob) GetAttachment(context.Context, string, string, string) (io.ReadCloser, blob.AttachmentMeta, error) {
	return nil, blob.AttachmentMeta{}, fmt.Errorf("nopBlob: not found")
}
func (nopBlob) PresignAttachment(context.Context, string, string, string, time.Duration) (string, error) {
	return "", fmt.Errorf("nopBlob: unsupported")
}
func (nopBlob) DeleteAttachment(context.Context, string, string, string) error { return nil }
func (nopBlob) DeleteRunAttachments(context.Context, string) error             { return nil }
func (nopBlob) PutToolBlob(context.Context, string, string, string, []byte) error {
	return nil
}
func (nopBlob) GetToolBlobRange(context.Context, string, string, string, int64, int64) ([]byte, int64, bool, error) {
	return nil, 0, false, blob.ErrArtifactNotFound
}
func (nopBlob) DeleteRunToolBlobs(context.Context, string) error { return nil }
func (nopBlob) PutRunFile(context.Context, string, string, string, io.Reader, int64) error {
	return nil
}
func (nopBlob) ListRunFiles(context.Context, string) ([]blob.RunFileObject, error) {
	return nil, nil
}
func (nopBlob) GetRunFile(context.Context, string, string) (io.ReadCloser, blob.RunFileObject, error) {
	return nil, blob.RunFileObject{}, blob.ErrArtifactNotFound
}
func (nopBlob) DeleteRunFiles(context.Context, string) error { return nil }
func (nopBlob) PutIRBlob(context.Context, string, []byte) error {
	return nil
}
func (nopBlob) GetIRBlob(context.Context, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (nopBlob) DeleteRunIR(context.Context, string) error { return nil }
func (nopBlob) PutBackendSession(context.Context, string, string, []byte) error {
	return nil
}
func (nopBlob) GetBackendSession(context.Context, string, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (nopBlob) DeleteBackendSession(context.Context, string, string) error { return nil }
func (nopBlob) DeleteRunBackendSessions(context.Context, string) error     { return nil }

var _ blob.Client = nopBlob{}
