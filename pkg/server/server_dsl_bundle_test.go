package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
)

// TestValidate_BundlePathMergesItsPrompts: a main.bot inside a bundle,
// validated with the file's workspace path, sees the bundle's prompts/*.md
// the way a launch does — the multi-file gallery shape declares no prompt
// in main.bot at all, and validated as a bare document it is refused
// twice (C003). Without the path the document alone is validated, as
// before the field existed.
func TestValidate_BundlePathMergesItsPrompts(t *testing.T) {
	srv, hs := newTestServer(t)

	tpl, ok := botscaffold.TemplateByID("multi-file")
	if !ok {
		t.Fatal("the multi-file template is gone")
	}
	spec := tpl.Spec
	spec.Slug = "mf"
	dir := filepath.Join(srv.cfg.WorkDir, "bots", spec.Slug)
	if _, err := botscaffold.Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	doc := parseDocument(t, hs.URL, string(src))

	validate := func(body string) validateResponse {
		t.Helper()
		resp := postDSLJSON(t, hs.URL+"/api/validate", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
		}
		var out validateResponse
		decodeJSONResp(t, resp, &out)
		return out
	}

	alone := validate(`{"document":` + string(doc) + `}`)
	var c003 int
	for _, iss := range alone.Issues {
		if iss.Code == "C003" {
			c003++
		}
	}
	if alone.Valid || c003 != 2 {
		t.Fatalf("validated alone: valid=%v, C003=%d, want invalid with the two prompt refs refused", alone.Valid, c003)
	}

	withPath := validate(`{"document":` + string(doc) + `,"path":"bots/mf/main.bot"}`)
	if !withPath.Valid {
		t.Fatalf("validated with its bundle path: diagnostics=%v", withPath.Diagnostics)
	}

	// A path the server cannot place, or one with a scheme, is a hint it
	// ignores: the document alone, never an error.
	for _, p := range []string{"../outside/main.bot", "botsource://team/mf/main.bot", "nowhere/main.bot"} {
		out := validate(`{"document":` + string(doc) + `,"path":"` + p + `"}`)
		if out.Valid {
			t.Errorf("path %q: the document validated as if its bundle were in scope", p)
		}
	}
}
