package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRunFilePreview_ServesImageFromExactRunWorkDir(t *testing.T) {
	srv, hs := newTestServer(t)
	runDir := t.TempDir()
	relPath := filepath.ToSlash(filepath.Join("renders", "final.png"))
	if err := os.MkdirAll(filepath.Join(runDir, "renders"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("\x89PNG\r\n\x1a\nrun-specific-preview")
	if err := os.WriteFile(filepath.Join(runDir, filepath.FromSlash(relPath)), want, 0o644); err != nil {
		t.Fatal(err)
	}
	seedRunWithWorkDir(t, srv, "preview-ok", runDir, true)

	resp, err := http.Get(hs.URL + "/api/runs/preview-ok/files/preview/renders/final.png")
	if err != nil {
		t.Fatalf("GET preview: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read preview: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestRunFilePreview_RejectsTraversalAndEscapingSymlink(t *testing.T) {
	srv, _ := newTestServer(t)
	runDir := t.TempDir()
	seedRunWithWorkDir(t, srv, "preview-safe", runDir, true)

	for _, bad := range []string{
		"../outside.png",
		"/etc/passwd.png",
		".git/config.png",
	} {
		t.Run(bad, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/runs/preview-safe/files/preview/file.png", nil)
			req.SetPathValue("id", "preview-safe")
			req.SetPathValue("path", bad)
			rec := httptest.NewRecorder()

			srv.handleGetRunFilePreview(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}

	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.png"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(runDir, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/runs/preview-safe/files/preview/escape/secret.png", nil)
	req.SetPathValue("id", "preview-safe")
	req.SetPathValue("path", "escape/secret.png")
	rec := httptest.NewRecorder()

	srv.handleGetRunFilePreview(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("escaping symlink status = %d, want 400", rec.Code)
	}
}

func TestRunFilePreview_RejectsMissingRunAndWorktree(t *testing.T) {
	srv, hs := newTestServer(t)

	resp, err := http.Get(hs.URL + "/api/runs/does-not-exist/files/preview/image.png")
	if err != nil {
		t.Fatalf("GET missing run: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run status = %d, want 404", resp.StatusCode)
	}

	seedRunWithWorkDir(t, srv, "no-workdir", "", true)
	resp, err = http.Get(hs.URL + "/api/runs/no-workdir/files/preview/image.png")
	if err != nil {
		t.Fatalf("GET run without workdir: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("no workdir status = %d, want 409", resp.StatusCode)
	}

	missingDir := filepath.Join(t.TempDir(), "removed-worktree")
	seedRunWithWorkDir(t, srv, "gone-worktree", missingDir, true)
	resp, err = http.Get(hs.URL + "/api/runs/gone-worktree/files/preview/image.png")
	if err != nil {
		t.Fatalf("GET gone worktree: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("gone worktree status = %d, want 409", resp.StatusCode)
	}
}
