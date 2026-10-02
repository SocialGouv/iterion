package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The C222 of /api/validate — "bundle does not open: …", a WARNING in the
// 200 body, not a refusal — names the manifest and the entrypoint the way
// the launch path's 422 does: relatively. The same broken bundle served
// the workdir twice until the hint error got the root cut its refusal
// sibling already had (#1970).
func TestValidateNamesAnUnopenableBundleWithoutTheHostRoot(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "bund"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "bund", "main.bot"), []byte("workflow x:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "bund", "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse("main.bot", "workflow x:\n  entry: done\n")
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	doc, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}

	s := newOrgTestServer(t)
	s.cfg.WorkDir = workDir
	body := `{"document":` + string(doc) + `,"path":"bund/main.bot"}`
	r := httptest.NewRequest(http.MethodPost, "/api/validate", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleValidate(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the 200 with the C222 warning: %s", w.Code, w.Body.String())
	}
	var out validateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(out.Warnings, "\n")
	if !strings.Contains(joined, "does not open") {
		t.Fatalf("the fixture is not the case under test (no C222 warning): %v", out.Warnings)
	}
	if strings.Contains(joined, workDir) {
		t.Errorf("the 200's warnings disclose the server's directory layout: %s", joined)
	}
}
