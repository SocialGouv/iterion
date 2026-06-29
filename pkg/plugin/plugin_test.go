package plugin

import (
	"testing"
)

func TestParseManifestValid(t *testing.T) {
	m, err := ParseManifest([]byte(`
name: demo
version: 1.0.0
default_enabled: true
contributes:
  rewriters:
    - id: demo
      locate: { bin: demo }
      invoke:
        argv: ["rewrite", "{{command}}"]
`))
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if m.Name != "demo" || !m.DefaultEnabled || len(m.Contributes.Rewriters) != 1 {
		t.Fatalf("parsed manifest wrong: %+v", m)
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]string{
		"no name":           "contributes:\n  skills: [a.md]\n",
		"contributes none":  "name: x\n",
		"rewriter no cmd":   "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\"] }\n",
		"mcp stdio no cmd":  "name: x\ncontributes:\n  mcp_servers:\n    - { name: s, transport: stdio }\n",
		"mcp bad transport": "name: x\ncontributes:\n  mcp_servers:\n    - { name: s, transport: carrier-pigeon, command: c }\n",
	}
	for label, doc := range cases {
		if _, err := ParseManifest([]byte(doc)); err == nil {
			t.Errorf("%s: expected error, got nil", label)
		}
	}
}

func TestLoadBuiltinsAndEnableState(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The three builtins must be present.
	for _, name := range []string{"rtk", "graphify", "repo-falcon"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("builtin %q missing", name)
		}
	}
	// rtk enabled by default; KG explorers disabled.
	if !reg.IsEnabled("rtk") {
		t.Error("rtk should be enabled by default")
	}
	if reg.IsEnabled("repo-falcon") {
		t.Error("repo-falcon should be disabled by default")
	}
	// Enable persists and survives a reload.
	if err := reg.SetEnabled("repo-falcon", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	reg2, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reg2.IsEnabled("repo-falcon") {
		t.Error("enable state did not persist across reload")
	}
	// A builtin cannot be uninstalled.
	if err := reg2.Remove("rtk"); err == nil {
		t.Error("removing a builtin should error")
	}
}

func TestEnabledRewritersAndSkills(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Only rtk is enabled by default and it is the lone rewriter.
	rw := reg.EnabledRewriters()
	if len(rw) != 1 || rw[0].Plugin != "rtk" {
		t.Fatalf("EnabledRewriters = %+v, want [rtk]", rw)
	}
	// repo-falcon's skill is readable from the embedded FS.
	p, _ := reg.Get("repo-falcon")
	files, err := p.SkillFiles()
	if err != nil {
		t.Fatalf("SkillFiles: %v", err)
	}
	if len(files) != 1 || files[0].Name != "code-knowledge-graph.md" || len(files[0].Content) == 0 {
		t.Fatalf("repo-falcon skill wrong: %+v", files)
	}
}

func TestExpandContext(t *testing.T) {
	e := ExpandContext{Workspace: "/ws", PluginDir: "/p", CacheDir: "/c"}
	got := e.Expand("serve {{workspace}}/.falcon {{plugin.cache}} {{plugin.dir}}")
	want := "serve /ws/.falcon /c /p"
	if got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
}
