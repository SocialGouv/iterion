package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *FilesystemRunStore {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestWriteAndOpenAttachment(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-001", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	body := []byte("hello world")
	rec := AttachmentRecord{
		Name:             "logo",
		OriginalFilename: "logo.png",
		MIME:             "image/png",
	}
	if err := s.WriteAttachment(ctx, "run-001", rec, bytes.NewReader(body)); err != nil {
		t.Fatalf("WriteAttachment: %v", err)
	}

	rc, got, err := s.OpenAttachment(ctx, "run-001", "logo")
	if err != nil {
		t.Fatalf("OpenAttachment: %v", err)
	}
	defer rc.Close()
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("body mismatch: %q", out)
	}
	if got.Size != int64(len(body)) {
		t.Errorf("size = %d want %d", got.Size, len(body))
	}
	if got.SHA256 == "" {
		t.Error("SHA256 not set")
	}
	if got.MIME != "image/png" {
		t.Errorf("MIME = %q want image/png", got.MIME)
	}
	if !strings.Contains(got.StorageRef, "runs/run-001/attachments/logo/logo.png") {
		t.Errorf("StorageRef = %q", got.StorageRef)
	}

	// Run.Attachments must be populated.
	r, err := s.LoadRun(ctx, "run-001")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if _, ok := r.Attachments["logo"]; !ok {
		t.Errorf("Run.Attachments[logo] missing; got %v", r.Attachments)
	}
}

func TestListAttachments(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-002", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := s.WriteAttachment(ctx, "run-002",
			AttachmentRecord{Name: name, OriginalFilename: name + ".bin", MIME: "application/octet-stream"},
			strings.NewReader("body-"+name),
		); err != nil {
			t.Fatalf("WriteAttachment %s: %v", name, err)
		}
	}
	list, err := s.ListAttachments(ctx, "run-002")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("expected 3 attachments, got %d", len(list))
	}
}

func TestDeleteRunAttachments(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-003", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.WriteAttachment(ctx, "run-003",
		AttachmentRecord{Name: "doc", OriginalFilename: "doc.txt", MIME: "text/plain"},
		strings.NewReader("hello"),
	); err != nil {
		t.Fatalf("WriteAttachment: %v", err)
	}
	if err := s.DeleteRunAttachments(ctx, "run-003"); err != nil {
		t.Fatalf("DeleteRunAttachments: %v", err)
	}
	list, err := s.ListAttachments(ctx, "run-003")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list after delete, got %v", list)
	}
	_, _, err = s.OpenAttachment(ctx, "run-003", "doc")
	if !errors.Is(err, ErrAttachmentNotFound) {
		t.Errorf("expected ErrAttachmentNotFound, got %v", err)
	}
}

func TestPresignAttachment_Roundtrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-004", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.WriteAttachment(ctx, "run-004",
		AttachmentRecord{Name: "img", OriginalFilename: "img.png", MIME: "image/png"},
		strings.NewReader("payload"),
	); err != nil {
		t.Fatalf("WriteAttachment: %v", err)
	}
	url, err := s.PresignAttachment(ctx, "run-004", "img", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignAttachment: %v", err)
	}
	if !strings.HasPrefix(url, "/api/runs/run-004/attachments/img?") {
		t.Errorf("unexpected URL shape: %q", url)
	}
	if !strings.Contains(url, "exp=") || !strings.Contains(url, "sig=") {
		t.Errorf("URL missing exp/sig: %q", url)
	}

	// Parse exp + sig back out and verify.
	q := url[strings.Index(url, "?")+1:]
	params := map[string]string{}
	for _, kv := range strings.Split(q, "&") {
		if i := strings.Index(kv, "="); i > 0 {
			params[kv[:i]] = kv[i+1:]
		}
	}
	if !s.VerifyAttachmentSignature("run-004", "img", params["exp"], params["sig"]) {
		t.Errorf("signature failed to verify")
	}
	// Tampering must fail.
	if s.VerifyAttachmentSignature("run-004", "other", params["exp"], params["sig"]) {
		t.Errorf("signature for different name unexpectedly verified")
	}
}

// A bytes file named exactly `.iterion` collides with the record
// sidecar's directory (staging.go): the write must fail loudly, leave
// nothing on disk, and never wedge the run's listing — a bundle default
// under that name reaches this path through promoteBundleAttachmentDefaults.
func TestWriteAttachmentReservedIterionNameFailsClean(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-res", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A good attachment beside the bad one: the listing must survive the
	// collision.
	if err := s.WriteAttachment(ctx, "run-res",
		AttachmentRecord{Name: "good", OriginalFilename: "good.bin", MIME: "application/octet-stream"},
		strings.NewReader("good"),
	); err != nil {
		t.Fatalf("WriteAttachment good: %v", err)
	}

	err := s.WriteAttachment(ctx, "run-res",
		AttachmentRecord{Name: "bad", OriginalFilename: ".iterion", MIME: "application/octet-stream"},
		strings.NewReader("x"))
	if err == nil {
		t.Fatal("WriteAttachment with OriginalFilename .iterion: expected an error, got nil")
	}

	// No residue: neither the bytes file nor a record under
	// attachments/bad/.
	badDir := s.attachmentDir("run-res", "bad")
	if _, lerr := os.Lstat(filepath.Join(badDir, ".iterion")); !os.IsNotExist(lerr) {
		t.Errorf("bytes residue at %s: lstat err = %v", filepath.Join(badDir, ".iterion"), lerr)
	}
	if _, lerr := os.Lstat(filepath.Join(badDir, ".iterion", "meta.json")); !os.IsNotExist(lerr) {
		t.Errorf("record residue at %s: lstat err = %v", filepath.Join(badDir, ".iterion", "meta.json"), lerr)
	}

	list, lerr := s.ListAttachments(ctx, "run-res")
	if lerr != nil {
		t.Fatalf("ListAttachments: %v", lerr)
	}
	if len(list) != 1 || list[0].Name != "good" {
		t.Errorf("ListAttachments = %+v, want only [good]", list)
	}
}

// A bytes file stranded at the record's directory — a crash mid-write,
// or a store written before the failed-write cleanup — reads as
// NOT-FOUND: it must not abort the whole listing.
func TestListAttachmentsSkipsIterionFileResidue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateRun(ctx, "run-res2", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.WriteAttachment(ctx, "run-res2",
		AttachmentRecord{Name: "good", OriginalFilename: "good.bin", MIME: "application/octet-stream"},
		strings.NewReader("good"),
	); err != nil {
		t.Fatalf("WriteAttachment good: %v", err)
	}

	badDir := s.attachmentDir("run-res2", "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, ".iterion"), []byte("x"), 0o644); err != nil {
		t.Fatalf("plant residue: %v", err)
	}

	if _, _, err := s.OpenAttachment(ctx, "run-res2", "bad"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Errorf("OpenAttachment on residue: err = %v, want ErrAttachmentNotFound", err)
	}
	list, err := s.ListAttachments(ctx, "run-res2")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != 1 || list[0].Name != "good" {
		t.Errorf("ListAttachments = %+v, want only [good]", list)
	}
}
