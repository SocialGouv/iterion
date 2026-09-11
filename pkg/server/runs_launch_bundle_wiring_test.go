package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestLaunchRun_FileLaunchReadsTheBundleFromTheOperatorsPath: the launch
// handler derives the bundle from the path the OPERATOR named, before the
// inline source is materialised under a name no promotion recognises. The
// proof is the refusal: a launch of `<bundle>/main.bot` with its source
// inline, on a bundle whose manifest does not decode, is refused by that
// manifest — which only the derivation from the operator's path can see.
// Without it the handler compiles the inline source alone and the same
// request fails on the prompt reference (C003), a different sentence.
func TestLaunchRun_FileLaunchReadsTheBundleFromTheOperatorsPath(t *testing.T) {
	srv, _ := newTestServer(t)
	tpl, ok := botscaffold.TemplateByID("multi-file")
	if !ok {
		t.Fatal("the multi-file template is gone")
	}
	spec := tpl.Spec
	spec.Slug = "mf"
	spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
	dir := filepath.Join(srv.cfg.WorkDir, "bots", spec.Slug)
	if _, err := botscaffold.Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.OpenDir(dir); err == nil {
		t.Fatal("the fixture manifest still opens; the case it guards is not exercised")
	}
	body, err := json.Marshal(map[string]any{
		"file_path": "bots/mf/main.bot",
		"source":    string(src),
		"run_id":    "run-bundle-wiring",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleLaunchRun(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "manifest") {
		t.Fatalf("status=%d body=%s; want 422 refusing the bundle by its manifest (a C003 here means the inline source was compiled alone)", rec.Code, rec.Body.String())
	}
}
