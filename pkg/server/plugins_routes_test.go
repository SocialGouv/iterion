package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// writePluginFixture installs a plugin manifest (plus any extra files) under
// the ITERION_HOME plugin tree the registry loads from. The caller must have
// pointed ITERION_HOME at a temp dir first (t.Setenv).
func writePluginFixture(t *testing.T, home, name, manifest string, extra map[string]string) {
	t.Helper()
	dir := filepath.Join(home, "plugins", name)
	files := map[string]string{plugin.ManifestFile: manifest}
	for rel, content := range extra {
		files[rel] = content
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newPluginServer(t *testing.T, workdir, mode string) *Server {
	t.Helper()
	srv := New(Config{
		DisableAuth: true,
		WorkDir:     workdir,
		Mode:        mode,
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	return srv
}

func TestPluginDetailRoute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	writePluginFixture(t, home, "detail-demo", `
name: detail-demo
description: a demo plugin
contributes:
  hooks:
    - hooks/pretooluse.json
  lifecycle:
    index: echo indexing
    refresh: echo refreshing
`, map[string]string{
		"hooks/pretooluse.json": `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "detail-demo hook '{{workspace}}'"}]}]}}`,
		"README.md":             "# detail-demo\n",
	})
	srv := newPluginServer(t, t.TempDir(), "")

	rec := doJSON(t, srv, "GET", "/api/v1/plugins/detail-demo", "")
	if rec.Code != 200 {
		t.Fatalf("detail: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The wire format is the snake_case JSON the studio types mirror.
	raw := rec.Body.String()
	for _, key := range []string{`"view"`, `"auto_index"`, `"hooks"`, `"event"`, `"lifecycle"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("detail JSON missing key %s: %s", key, raw)
		}
	}
	var d struct {
		View struct {
			Name string `json:"name"`
		} `json:"view"`
		Readme string `json:"readme"`
		Hooks  []struct {
			Event    string   `json:"event"`
			Commands []string `json:"commands"`
		} `json:"hooks"`
		Lifecycle *struct {
			Index   string `json:"index"`
			Refresh string `json:"refresh"`
		} `json:"lifecycle"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if d.View.Name != "detail-demo" || d.Readme != "# detail-demo\n" {
		t.Errorf("view/readme wrong: %+v", d)
	}
	if len(d.Hooks) != 1 || d.Hooks[0].Event != "PreToolUse" ||
		len(d.Hooks[0].Commands) != 1 || d.Hooks[0].Commands[0] != "detail-demo hook '{{workspace}}'" {
		t.Errorf("hooks = %+v", d.Hooks)
	}
	if d.Lifecycle == nil || d.Lifecycle.Index != "echo indexing" || d.Lifecycle.Refresh != "echo refreshing" {
		t.Errorf("lifecycle = %+v", d.Lifecycle)
	}
}

func TestPluginDetailNotFound(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	srv := newPluginServer(t, t.TempDir(), "")
	rec := doJSON(t, srv, "GET", "/api/v1/plugins/no-such-plugin", "")
	if rec.Code != 404 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPluginLifecycleRoute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	writePluginFixture(t, home, "lc-demo", `
name: lc-demo
contributes:
  lifecycle:
    index: touch lc-marker && echo indexed {{workspace}}
    refresh: echo boom >&2; exit 3
`, nil)
	workdir := t.TempDir()
	srv := newPluginServer(t, workdir, "")

	type lcResp struct {
		Name      string `json:"name"`
		Phase     string `json:"phase"`
		OK        bool   `json:"ok"`
		Output    string `json:"output"`
		Truncated bool   `json:"truncated"`
		Error     string `json:"error"`
	}

	// Success: the command runs in WorkDir (marker lands there) and its
	// stdout is captured.
	rec := doJSON(t, srv, "POST", "/api/v1/plugins/lc-demo/lifecycle/index", "")
	if rec.Code != 200 {
		t.Fatalf("index: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res lcResp
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.OK || res.Name != "lc-demo" || res.Phase != "index" || res.Error != "" {
		t.Errorf("index result = %+v", res)
	}
	if !strings.Contains(res.Output, "indexed "+workdir) {
		t.Errorf("output missing expanded workspace: %q", res.Output)
	}
	if _, err := os.Stat(filepath.Join(workdir, "lc-marker")); err != nil {
		t.Errorf("marker not written in WorkDir: %v", err)
	}

	// A command that ran and failed is a 200 with ok:false + captured output.
	rec = doJSON(t, srv, "POST", "/api/v1/plugins/lc-demo/lifecycle/refresh", "")
	if rec.Code != 200 {
		t.Fatalf("refresh: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	res = lcResp{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Errorf("refresh result should fail with an error: %+v", res)
	}
	if !strings.Contains(res.Output, "boom") {
		t.Errorf("stderr not captured: %q", res.Output)
	}
}

func TestPluginLifecycleRejectedInCloudMode(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	srv := newPluginServer(t, t.TempDir(), "cloud")
	rec := doJSON(t, srv, "POST", "/api/v1/plugins/anything/lifecycle/index", "")
	if rec.Code != 403 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cloud mode") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestPluginLifecycleInvalidPhase(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	srv := newPluginServer(t, t.TempDir(), "")
	rec := doJSON(t, srv, "POST", "/api/v1/plugins/anything/lifecycle/compact", "")
	if rec.Code != 400 {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "index|refresh") {
		t.Errorf("body = %s", rec.Body.String())
	}
}
