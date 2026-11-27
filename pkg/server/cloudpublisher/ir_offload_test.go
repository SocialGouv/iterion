package cloudpublisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// fakeIRStore is a RunStore that also satisfies store.IRBlobStore, capturing
// offloaded IR bytes in memory so the offload path is testable without S3.
// It derives keys like the cloud store (blob.IRBlobKey) and, like S3,
// replaces the bytes when a PUT lands on an existing key.
type fakeIRStore struct {
	store.RunStore
	blobs   map[string][]byte
	backend string
}

func (f *fakeIRStore) PutIRBlob(_ context.Context, logicalID string, body []byte) (string, error) {
	key, err := blob.IRBlobKey(logicalID)
	if err != nil {
		return "", err
	}
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
	if msg.IRRef.Digest != queue.IRDigest(ir) {
		t.Fatalf("digest = %q, want the SHA-256 of the offloaded bytes %q", msg.IRRef.Digest, queue.IRDigest(ir))
	}
	contentKey := regexp.MustCompile(`^ir/run-big-bundle-ir-` + queue.IRDigest(ir) + `-[0-9a-f]{32}\.json$`)
	if !contentKey.MatchString(msg.IRRef.StorageKey) {
		t.Fatalf("storage key = %q, want the content key ir/run-big-bundle-ir-<sha256>-<nonce>.json", msg.IRRef.StorageKey)
	}
	if id, err := blob.IRBlobKeyRunID(msg.IRRef.StorageKey); err != nil || !strings.HasPrefix(id, "run-big"+store.IRBlobFamilyInfix) {
		t.Fatalf("IRBlobKeyRunID(%q) = %q, %v: a runner refuses the key, or reads another run's family", msg.IRRef.StorageKey, id, err)
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

// A later publication of the same run — a resume, a re-publish after a
// reverted queued flip — must not replace the program an earlier message
// still points at: each publication's IR lands under its own content key,
// with its own digest, and the earlier key still serves the earlier bytes
// once the later publication is stored. Two publications of identical
// bytes get two keys too: the nonce, not the bytes, makes the key the
// publication's own.
func TestOffloadOversizedIR_LaterPublicationLeavesTheEarlierProgram(t *testing.T) {
	fake := &fakeIRStore{}
	p := testPublisher(fake, 64*1024)
	programs := []json.RawMessage{
		json.RawMessage(`{"pad":"` + strings.Repeat("a", 200*1024) + `"}`),
		json.RawMessage(`{"pad":"` + strings.Repeat("b", 200*1024) + `"}`),
		json.RawMessage(`{"pad":"` + strings.Repeat("b", 200*1024) + `"}`),
	}
	var refs []*queue.IRRef
	for _, ir := range programs {
		msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "same-run", WorkflowName: "wf", IRCompiled: ir}
		if err := p.offloadOversizedIR(context.Background(), msg); err != nil {
			t.Fatalf("offload: %v", err)
		}
		if msg.IRRef == nil {
			t.Fatal("oversized IR not offloaded")
		}
		if err := msg.Validate(); err != nil {
			t.Fatalf("offloaded message fails Validate: %v", err)
		}
		refs = append(refs, msg.IRRef)
	}
	seen := map[string]int{}
	for i, ref := range refs {
		if j, dup := seen[ref.StorageKey]; dup {
			t.Fatalf("publications %d and %d both point at %s: the later one overwrote the object the earlier message names", j, i, ref.StorageKey)
		}
		seen[ref.StorageKey] = i
	}
	for i, ref := range refs {
		got, err := fake.GetIRBlob(context.Background(), ref.StorageKey)
		if err != nil {
			t.Fatalf("GetIRBlob(%s): %v", ref.StorageKey, err)
		}
		if !bytes.Equal(got, programs[i]) {
			t.Fatalf("publication %d's key %s serves another program", i, ref.StorageKey)
		}
		if ref.Digest != queue.IRDigest(programs[i]) {
			t.Fatalf("publication %d's digest = %q, want %q", i, ref.Digest, queue.IRDigest(programs[i]))
		}
	}
}

// The content id's nonce is what no caller can predict: a nonce source that
// fails refuses the offload with its cause — and the publication with it —
// instead of naming the blob with a predictable id. Nothing is stored and
// the message is left as it was.
func TestOffloadOversizedIR_NonceSourceFailureRefusesTheOffload(t *testing.T) {
	fake := &fakeIRStore{}
	p := testPublisher(fake, 64*1024)
	cause := errors.New("entropy source unavailable")
	p.irNonceSource = iotest.ErrReader(cause)
	ir := bigIR(200 * 1024)
	msg := &queue.RunMessage{V: queue.SchemaVersion, RunID: "run-big", WorkflowName: "wf", IRCompiled: ir}
	err := p.offloadOversizedIR(context.Background(), msg)
	if !errors.Is(err, cause) {
		t.Fatalf("offload with a failing nonce source = %v, want a refusal wrapping %v", err, cause)
	}
	if len(fake.blobs) != 0 {
		t.Fatalf("the refused offload stored %d blob(s): %v", len(fake.blobs), fake.blobs)
	}
	if msg.IRRef != nil || !bytes.Equal(msg.IRCompiled, ir) {
		t.Fatalf("the refused offload changed the message: ir_ref=%+v, inline IR kept=%t", msg.IRRef, bytes.Equal(msg.IRCompiled, ir))
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
