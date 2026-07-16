package cloudpublisher

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeIRStore is a RunStore that also satisfies store.IRBlobStore, capturing
// offloaded IR bytes in memory so the offload path is testable without S3.
type fakeIRStore struct {
	store.RunStore
	blobs   map[string][]byte
	backend string
}

func (f *fakeIRStore) PutIRBlob(_ context.Context, runID string, body []byte) (string, error) {
	key := "ir/" + runID + ".json"
	if f.blobs == nil {
		f.blobs = map[string][]byte{}
	}
	f.blobs[key] = append([]byte{}, body...)
	return key, nil
}

func (f *fakeIRStore) GetIRBlob(_ context.Context, storageKey string) ([]byte, error) {
	b, ok := f.blobs[storageKey]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte{}, b...), nil
}

func (f *fakeIRStore) IRBlobBackend() string {
	if f.backend != "" {
		return f.backend
	}
	return "s3"
}

func testPublisher(st store.RunStore, maxPayload int64) *Publisher {
	return &Publisher{
		store:      st,
		logger:     iterlog.New(iterlog.LevelError, io.Discard),
		maxPayload: func() int64 { return maxPayload },
	}
}

func bigIR(n int) json.RawMessage {
	return json.RawMessage(`{"pad":"` + strings.Repeat("x", n) + `"}`)
}

func TestOffloadOversizedIR_SmallStaysInline(t *testing.T) {
	fake := &fakeIRStore{}
	p := testPublisher(fake, 1<<20) // 1 MiB
	ir := bigIR(1024)
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "run-small", WorkflowName: "wf", IRCompiled: ir}
	if err := p.offloadOversizedIR(context.Background(), msg); err != nil {
		t.Fatalf("offload: %v", err)
	}
	if !bytes.Equal(msg.IRCompiled, ir) {
		t.Fatal("small IR should stay inline")
	}
	if msg.IRRef != nil {
		t.Fatalf("small IR should not offload, got ref %+v", msg.IRRef)
	}
	if len(fake.blobs) != 0 {
		t.Fatalf("nothing should be stashed, got %d blobs", len(fake.blobs))
	}
}

func TestOffloadOversizedIR_LargeOffloads(t *testing.T) {
	fake := &fakeIRStore{}
	const maxPayload = 64 * 1024
	p := testPublisher(fake, maxPayload)
	ir := bigIR(200 * 1024) // well over the limit
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "run-big", WorkflowName: "wf", IRCompiled: ir}
	if err := p.offloadOversizedIR(context.Background(), msg); err != nil {
		t.Fatalf("offload: %v", err)
	}
	if msg.IRCompiled != nil {
		t.Fatal("oversized IR should be cleared from the message")
	}
	if msg.IRRef == nil {
		t.Fatal("oversized IR should produce an IRRef")
	}
	if msg.IRRef.Backend != queue.IRBackendS3 {
		t.Fatalf("backend = %q, want s3", msg.IRRef.Backend)
	}
	if msg.IRRef.StorageKey != "ir/run-big.json" {
		t.Fatalf("storage key = %q", msg.IRRef.StorageKey)
	}
	// The message must now validate and fit.
	if err := msg.Validate(); err != nil {
		t.Fatalf("offloaded message fails Validate: %v", err)
	}
	body, _ := json.Marshal(msg)
	if int64(len(body)) > maxPayload {
		t.Fatalf("offloaded message still %d bytes (> %d)", len(body), maxPayload)
	}
	// Round-trips: the runner can fetch the exact bytes back by key.
	got, err := fake.GetIRBlob(context.Background(), msg.IRRef.StorageKey)
	if err != nil {
		t.Fatalf("GetIRBlob: %v", err)
	}
	if !bytes.Equal(got, ir) {
		t.Fatal("stashed IR bytes differ from the original")
	}
}

func TestOffloadOversizedIR_NoSeamFailsLoudly(t *testing.T) {
	// A store without the IRBlobStore seam must error, not silently
	// truncate the IR or publish an oversized message.
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := testPublisher(st, 64*1024)
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "run-x", WorkflowName: "wf", IRCompiled: bigIR(200 * 1024)}
	err = p.offloadOversizedIR(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "cannot host an out-of-band IR blob") {
		t.Fatalf("expected out-of-band failure, got %v", err)
	}
}

func TestOffloadOversizedIR_UnknownBackendRejected(t *testing.T) {
	fake := &fakeIRStore{backend: "filesystem"}
	p := testPublisher(fake, 64*1024)
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "run-y", WorkflowName: "wf", IRCompiled: bigIR(200 * 1024)}
	err := p.offloadOversizedIR(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "unsupported backend") {
		t.Fatalf("expected unsupported-backend error, got %v", err)
	}
}

func TestOffloadOversizedIR_NoMaxPayloadIsNoop(t *testing.T) {
	fake := &fakeIRStore{}
	p := &Publisher{store: fake, logger: iterlog.New(iterlog.LevelError, io.Discard)} // maxPayload nil
	ir := bigIR(200 * 1024)
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "run-z", WorkflowName: "wf", IRCompiled: ir}
	if err := p.offloadOversizedIR(context.Background(), msg); err != nil {
		t.Fatalf("offload: %v", err)
	}
	if msg.IRRef != nil || !bytes.Equal(msg.IRCompiled, ir) {
		t.Fatal("with no max_payload the message must be untouched")
	}
}
