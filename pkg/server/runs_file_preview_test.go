package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunWorktreePreviewImageTypeAllowlist(t *testing.T) {
	for path, want := range map[string]string{
		"image.png":  "image/png",
		"image.jpg":  "image/jpeg",
		"image.JPEG": "image/jpeg",
		"image.webp": "image/webp",
		"image.gif":  "image/gif",
	} {
		t.Run(path, func(t *testing.T) {
			got, ok := runWorktreePreviewImageType(path)
			if !ok || got != want {
				t.Fatalf("runWorktreePreviewImageType(%q) = (%q, %v), want (%q, true)", path, got, ok, want)
			}
		})
	}
}

func TestRunWorktreeImagePreviewServesPassiveImageAndRange(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	seedRunWithWorkDir(t, srv, "preview-image", dir, true)

	reviewDir := filepath.Join(dir, "review")
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("\x89PNG\r\npreview-bytes")
	if err := os.WriteFile(filepath.Join(reviewDir, "final.png"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	url := hs.URL + "/api/runs/preview-image/files/preview/review/final.png"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("body = %q, want %q", body, payload)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") ||
		!strings.Contains(got, "final.png") {
		t.Errorf("Content-Disposition = %q, want inline final.png", got)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=1-3")
	ranged, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, readErr := io.ReadAll(ranged.Body)
	ranged.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if ranged.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", ranged.StatusCode)
	}
	if got := ranged.Header.Get("Content-Range"); got != "bytes 1-3/19" {
		t.Errorf("Content-Range = %q, want bytes 1-3/19", got)
	}
	if !bytes.Equal(rangeBody, payload[1:4]) {
		t.Errorf("range body = %q, want %q", rangeBody, payload[1:4])
	}
}

func TestRunWorktreeImagePreviewRejectsNonPassiveTypes(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	seedRunWithWorkDir(t, srv, "preview-types", dir, true)

	for name, body := range map[string]string{
		"secret.txt":  "not an image",
		"active.svg":  "<svg/>",
		"active.html": "<script>alert(1)</script>",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(hs.URL + "/api/runs/preview-types/files/preview/" + name)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", name, resp.StatusCode)
		}
	}
}

func TestRunWorktreeImagePreviewRejectsTraversalAndEscapingSymlink(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := initRepo(t)
	seedRunWithWorkDir(t, srv, "preview-paths", dir, true)

	// SetPathValue calls the handler with the exact wildcard value, bypassing
	// net/http's URL dot-segment canonicalization so ValidateRelPath itself is
	// exercised.
	req := httptest.NewRequest(http.MethodGet, "/api/runs/preview-paths/files/preview/escape.png", nil)
	req.SetPathValue("id", "preview-paths")
	req.SetPathValue("path", "../../outside.png")
	rec := httptest.NewRecorder()
	srv.handlePreviewRunWorktreeImage(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal status = %d, want 400", rec.Code)
	}

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(secret, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.png")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(hs.URL + "/api/runs/preview-paths/files/preview/leak.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("escaping symlink status = %d, want 400", resp.StatusCode)
	}
}
