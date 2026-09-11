package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestValidate_SaysWhenAManifestBesideMainBotWasNotRead: a main.bot beside
// a `manifest.yaml` that does NOT mark its bundle — a typo in the only
// distinctive key — validates alone, and the response SAYS so (C223,
// naming the key): the one outcome that would otherwise be silent, since
// the file compiles and nothing names the prompts, presets and skills it
// lost. A manifest that marks emits nothing of the kind.
func TestValidate_SaysWhenAManifestBesideMainBotWasNotRead(t *testing.T) {
	srv, hs := newTestServer(t)
	dir := filepath.Join(srv.cfg.WorkDir, "bots", "typo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "workflow typo:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: typo\ndisplay_nmae: Typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bundle.DirForMainBot(filepath.Join(dir, "main.bot")); got != "" {
		t.Fatalf("the fixture manifest marks (%q); the case it guards is not exercised", got)
	}
	doc := parseDocument(t, hs.URL, src)
	validate := func() validateResponse {
		t.Helper()
		resp := postDSLJSON(t, hs.URL+"/api/validate", `{"document":`+string(doc)+`,"path":"bots/typo/main.bot"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, mustReadBody(t, resp))
		}
		var out validateResponse
		decodeJSONResp(t, resp, &out)
		return out
	}
	out := validate()
	var c223 int
	for _, iss := range out.Issues {
		if iss.Code != "C223" {
			continue
		}
		c223++
		if iss.Severity != "warning" || !strings.Contains(iss.Message, "display_nmae") {
			t.Errorf("C223 = %+v, want a warning naming the unknown key", iss)
		}
	}
	if c223 != 1 {
		t.Fatalf("the response carries %d C223 warnings, want exactly one saying the manifest beside main.bot was not read: %+v", c223, out.Issues)
	}
	// Fix the typo: the manifest marks, the bundle opens, nothing to say.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: typo\ndisplay_name: Typo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, iss := range validate().Issues {
		if iss.Code == "C223" {
			t.Fatalf("a manifest that marks still reported as not read: %+v", iss)
		}
	}
}
