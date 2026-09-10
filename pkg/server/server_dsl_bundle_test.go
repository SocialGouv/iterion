package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
	"github.com/SocialGouv/iterion/pkg/bundle"
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

	// A sibling manifest.yaml that does not open leaves the editor with the
	// document's OWN diagnostics, never an error for the whole request: the
	// studio edits manifests too, so half-typed is a normal state, and
	// useAutoValidation only console.warns a failure — the screen would keep
	// stale diagnostics for as long as the file is broken.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.OpenDir(dir); err == nil {
		t.Fatal("the fixture manifest still opens; the case it guards is not exercised")
	}
	resp := postDSLJSON(t, hs.URL+"/api/validate", `{"document":`+string(doc)+`,"path":"bots/mf/main.bot"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a broken sibling manifest answered %d for the whole request; body=%s", resp.StatusCode, mustReadBody(t, resp))
	}
	var broken validateResponse
	decodeJSONResp(t, resp, &broken)
	if len(broken.Issues) == 0 {
		t.Errorf("the response carries no diagnostics of its own: %+v", broken)
	}
}
