package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func uploadTestServer(t *testing.T) *Server {
	t.Helper()
	storeDir := t.TempDir()
	cfg := Config{
		Port:               -1,
		WorkDir:            t.TempDir(),
		StoreDir:           storeDir,
		Mode:               "web",
		MaxUploadSize:      1 << 20, // 1 MB
		MaxTotalUploadSize: 5 << 20, // 5 MB
		MaxUploadsPerRun:   3,
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	srv := New(cfg, logger)
	if srv.runs == nil {
		t.Skip("runview service unavailable in this build")
	}
	return srv
}

func multipartBody(t *testing.T, filename, contentType string, body []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if contentType != "" {
		_ = mw.WriteField("declared_mime", contentType)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestUploadAttachment_HappyPath(t *testing.T) {
	srv := uploadTestServer(t)

	body, ct := multipartBody(t, "logo.png",
		"image/png",
		// PNG magic bytes + a few bytes so http.DetectContentType sniffs image/png.
		append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("rest")...),
	)

	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", body)
	r.Header.Set("Content-Type", ct)

	w := httptest.NewRecorder()

	srv.handleUploadAttachment(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp stagedUpload
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.UploadID, "up_") {
		t.Errorf("unexpected upload_id: %q", resp.UploadID)
	}
	if resp.MIME != "image/png" {
		t.Errorf("mime = %q, want image/png", resp.MIME)
	}
	if resp.Size == 0 {
		t.Errorf("size = 0")
	}
	// Staged file must exist on disk.
	dir := filepath.Join(srv.runs.StoreRoot(), uploadStagingSubdir, resp.UploadID)
	if _, err := os.Stat(filepath.Join(dir, "logo.png")); err != nil {
		t.Errorf("staged file missing: %v", err)
	}
}

func TestUploadAttachment_TooLarge(t *testing.T) {
	srv := uploadTestServer(t)
	srv.cfg.MaxUploadSize = 64 // bytes
	body, ct := multipartBody(t, "big.txt", "text/plain", bytes.Repeat([]byte{'A'}, 4096))

	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", body)
	r.Header.Set("Content-Type", ct)

	w := httptest.NewRecorder()

	srv.handleUploadAttachment(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestUploadAttachment_DisallowedMIME(t *testing.T) {
	srv := uploadTestServer(t)
	srv.cfg.AllowedUploadMIMEs = []string{"image/png"}
	body, ct := multipartBody(t, "evil.exe", "application/octet-stream",
		// MZ header — http.DetectContentType identifies as
		// application/octet-stream which is not allowlisted.
		[]byte{0x4D, 0x5A, 0x90, 0x00, 0x03},
	)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", body)
	r.Header.Set("Content-Type", ct)

	w := httptest.NewRecorder()

	srv.handleUploadAttachment(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestServerInfo(t *testing.T) {
	srv := uploadTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/server/info", nil)

	w := httptest.NewRecorder()
	srv.handleServerInfo(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var info serverInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Mode != "web" {
		t.Errorf("mode = %q want web", info.Mode)
	}
	if info.Limits.Upload.MaxFileSize != 1<<20 {
		t.Errorf("max_file_size = %d", info.Limits.Upload.MaxFileSize)
	}
}

func TestMimeAllowed(t *testing.T) {
	tests := []struct {
		mime string
		pat  []string
		want bool
	}{
		{"image/png", []string{"image/*"}, true},
		{"image/png", []string{"image/png"}, true},
		{"image/png", []string{"text/*"}, false},
		{"application/octet-stream", []string{"application/json"}, false},
		{"text/plain; charset=utf-8", []string{"text/plain"}, true},
		{"image/PNG", []string{"image/png"}, true},
	}
	for _, tc := range tests {
		got := mimeAllowed(tc.mime, tc.pat)
		if got != tc.want {
			t.Errorf("mimeAllowed(%q,%v) = %v want %v", tc.mime, tc.pat, got, tc.want)
		}
	}
}

func TestPromoteStaged(t *testing.T) {
	srv := uploadTestServer(t)
	srv.cfg.AllowedUploadMIMEs = []string{"text/*", "image/*", "application/octet-stream"}

	// First upload -> staging
	body, ct := multipartBody(t, "data.txt", "text/plain", []byte("hello world"))
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/runs/uploads", body)
	r.Header.Set("Content-Type", ct)

	w := httptest.NewRecorder()
	srv.handleUploadAttachment(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("upload status %d", w.Code)
	}
	var staged stagedUpload
	_ = json.NewDecoder(w.Body).Decode(&staged)

	// Manually seed run.json under the FS store root so promoteStaged
	// has somewhere to write the attachment.
	runID := "run-promote-001"
	rootDir := srv.runs.StoreRoot()
	runDir := filepath.Join(rootDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir runDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"),
		[]byte(`{"id":"`+runID+`","format_version":1,"workflow_name":"wf","status":"running","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"}`),
		0o644); err != nil {
		t.Fatalf("write run.json: %v", err)
	}

	mapping := map[string]string{"data": staged.UploadID}
	out, _, err := srv.promoteStaged(r.Context(), runID, mapping)
	if err != nil {
		t.Fatalf("promoteStaged: %v", err)
	}
	if rec, ok := out["data"]; !ok || rec.Size == 0 {
		t.Errorf("output missing or empty: %+v", out)
	}
	// Staging dir for that upload must be gone.
	if _, err := os.Stat(filepath.Join(rootDir, uploadStagingSubdir, staged.UploadID)); !os.IsNotExist(err) {
		t.Errorf("staging dir not cleaned: %v", err)
	}
	// And the run must have the attachment persisted (read back via store).
	stored, _, err := srv.runs.OpenAttachment(r.Context(), runID, "data")
	if err != nil {
		t.Fatalf("OpenAttachment: %v", err)
	}
	defer stored.Close()
	got, _ := io.ReadAll(stored)
	if string(got) != "hello world" {
		t.Errorf("body = %q", got)
	}
}
