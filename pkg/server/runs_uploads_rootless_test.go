package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2086. The cloud server has no filesystem root, and the upload handler
// answered 503 there ("store has no filesystem root"). Staging is the run
// store's capability now: the handler stages through store.StagingStore
// and never touches a disk, so a store without a root that stages uploads
// serves the launch form, and a store that stages nothing is refused typed.

// memStaging is a StagingStore in memory — the contract both real twins
// pass in storetest, held by a store with no root.
type memStaging struct {
	mu     sync.Mutex
	recs   map[string]store.StagedUpload
	bodies map[string][]byte
}

func newMemStaging() *memStaging {
	return &memStaging{recs: map[string]store.StagedUpload{}, bodies: map[string][]byte{}}
}

func (m *memStaging) StageUpload(_ context.Context, rec store.StagedUpload, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != rec.Size {
		return errors.New("memStaging: the body is not the record's size")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[rec.UploadID] = rec
	m.bodies[rec.UploadID] = data
	return nil
}

func (m *memStaging) OpenStagedUpload(_ context.Context, uploadID string) (store.StagedUpload, io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[uploadID]
	if !ok {
		return store.StagedUpload{}, nil, store.ErrUploadNotFound
	}
	return rec, io.NopCloser(bytes.NewReader(m.bodies[uploadID])), nil
}

func (m *memStaging) DiscardStagedUpload(_ context.Context, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, uploadID)
	delete(m.bodies, uploadID)
	return nil
}

func (m *memStaging) ReapStagedUploads(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, rec := range m.recs {
		if rec.CreatedAt.Before(cutoff) {
			delete(m.recs, id)
			delete(m.bodies, id)
			n++
		}
	}
	return n, nil
}

func (m *memStaging) holds(uploadID string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bodies[uploadID]
	return b, ok
}

// rootlessStore is a run store with no filesystem root — the cloud shape —
// that stages uploads. Attachments persist through the wrapped store.
type rootlessStore struct {
	store.RunStore
	staging *memStaging
}

func (r *rootlessStore) Root() string { return "" }
func (r *rootlessStore) StageUpload(ctx context.Context, rec store.StagedUpload, body io.Reader) error {
	return r.staging.StageUpload(ctx, rec, body)
}
func (r *rootlessStore) OpenStagedUpload(ctx context.Context, id string) (store.StagedUpload, io.ReadCloser, error) {
	return r.staging.OpenStagedUpload(ctx, id)
}
func (r *rootlessStore) DiscardStagedUpload(ctx context.Context, id string) error {
	return r.staging.DiscardStagedUpload(ctx, id)
}
func (r *rootlessStore) ReapStagedUploads(ctx context.Context, cutoff time.Time) (int, error) {
	return r.staging.ReapStagedUploads(ctx, cutoff)
}

// bareStore has no root and stages nothing: the method set of the embedded
// INTERFACE carries no staging, so the capability probe answers nil.
type bareStore struct{ store.RunStore }

func (bareStore) Root() string { return "" }

func serverOn(t *testing.T, backing store.RunStore) *Server {
	t.Helper()
	srv := uploadTestServer(t)
	srv.runs = newTestRunviewService(t, "", runview.WithStore(backing), runview.WithLogger(iterlog.New(iterlog.LevelError, os.Stderr)))
	if srv.runs.StoreRoot() != "" {
		t.Fatal("the fixture has a filesystem root: the witness would prove nothing")
	}
	return srv
}

var pdfBytes = append([]byte("%PDF-1.5\n"), bytes.Repeat([]byte("staged "), 8)...)

func TestUploadAttachment_StagesThroughTheStoreWhenItHasNoFilesystemRoot(t *testing.T) {
	fsStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staging := newMemStaging()
	srv := serverOn(t, &rootlessStore{RunStore: fsStore, staging: staging})

	body, ct := multipartBody(t, "brief.pdf", "application/pdf", pdfBytes)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	srv.handleUploadAttachment(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s — a store with no root cannot stage?", w.Code, w.Body.String())
	}
	var staged stagedUpload
	if err := json.NewDecoder(w.Body).Decode(&staged); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if staged.MIME != "application/pdf" || staged.Size != int64(len(pdfBytes)) || staged.SHA256 == "" {
		t.Fatalf("the record is not the upload's: %+v", staged)
	}
	if held, ok := staging.holds(staged.UploadID); !ok || !bytes.Equal(held, pdfBytes) {
		t.Fatalf("the bytes did not reach the staging seam: held=%v equal=%v", ok, bytes.Equal(held, pdfBytes))
	}
	if len(w.Body.String()) > 0 && bytes.Contains(w.Body.Bytes(), []byte("tenant")) {
		t.Fatalf("the wire answer carries a tenant field: %s", w.Body.String())
	}

	// The launch promotes it to a run attachment and the staging is cleared.
	ctx := context.Background()
	const runID = "run-rootless-001"
	if _, err := fsStore.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	out, total, err := srv.promoteStaged(ctx, runID, map[string]string{"brief": staged.UploadID})
	if err != nil {
		t.Fatalf("promoteStaged: %v", err)
	}
	if total != int64(len(pdfBytes)) || out["brief"].SHA256 != staged.SHA256 {
		t.Fatalf("promoted %+v (total %d), want the staged record", out, total)
	}
	rc, _, err := srv.runs.OpenAttachment(ctx, runID, "brief")
	if err != nil {
		t.Fatalf("OpenAttachment: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, pdfBytes) {
		t.Fatalf("the attachment's bytes are not the upload's")
	}
	if _, ok := staging.holds(staged.UploadID); ok {
		t.Fatal("the staged upload survived its promotion")
	}
	// A second promotion of the same id is the typed not-found of before.
	if _, _, err := srv.promoteStaged(ctx, "run-rootless-002", map[string]string{"again": staged.UploadID}); err == nil || err.Error() != `upload "`+staged.UploadID+`" not found` {
		t.Fatalf("re-promotion = %v, want the not-found", err)
	}
}

func TestUploadAttachment_IsRefusedTypedWhenTheStoreStagesNothing(t *testing.T) {
	fsStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := serverOn(t, bareStore{RunStore: fsStore})
	// The reason is typed at the seam; a 5xx body is masked on the wire by
	// design, so it is read here, where the handler reads it.
	if _, err := srv.stagingStore(); err == nil || !strings.Contains(err.Error(), "stages no uploads") {
		t.Fatalf("stagingStore() = %v, want the typed refusal naming the missing seam", err)
	}
	body, ct := multipartBody(t, "brief.pdf", "application/pdf", pdfBytes)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	srv.handleUploadAttachment(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body=%s; want 503", w.Code, w.Body.String())
	}
}
