package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestSaveRejectsMaterializedBotDependency(t *testing.T) {
	workdir := t.TempDir()
	path := filepath.Join(workdir, ".botz", "shared-planner", "main.bot")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("workflow shared:\n  entry: start\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}
	body := []byte(`{"path":".botz/shared-planner/main.bot","document":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	s.handleSaveFile(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s; want 403", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "workflow shared:\n  entry: start\n" {
		t.Fatalf("materialized dependency was modified: %q", got)
	}
}

func TestFileDependencyMetadataUsesLockAndBundleManifest(t *testing.T) {
	workdir := t.TempDir()
	bundleDir := filepath.Join(workdir, ".botz", "shared-planner")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bundleDir, "main.bot"), "workflow shared:\n  entry: start\n")
	writeTestFile(t, filepath.Join(bundleDir, "manifest.yaml"), `name: shared-planner
version: 0.1.0
schema_version: 1
enabled: false
exports:
  workflows:
    - id: hierarchy-feature-author
      path: main.bot
`)
	hash, err := bundle.ContentHashDir(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(workdir, "bots.lock"), fmt.Sprintf(`version: 1
dependencies:
  shared-planner:
    source: https://example.invalid/shared.git
    ref: deadbeef
    path: bots/shared-planner
    bundle_sha256: %s
`, hash))
	s := &Server{cfg: Config{WorkDir: workdir}}
	req := httptest.NewRequest(http.MethodPost, "/api/files/dependency", bytes.NewReader(
		[]byte(`{"path":".botz/shared-planner/main.bot"}`),
	))
	rec := httptest.NewRecorder()

	s.handleFileDependency(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got fileDependencyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ReadOnly || got.SharedBundle == nil {
		t.Fatalf("metadata = %#v", got)
	}
	if got.SharedBundle.Name != "shared-planner" ||
		got.SharedBundle.Version != "0.1.0" ||
		got.SharedBundle.Workflow != "hierarchy-feature-author" ||
		got.SharedBundle.BundleSHA256 != hash ||
		!got.SharedBundle.Verified {
		t.Fatalf("shared bundle metadata = %#v", got.SharedBundle)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
