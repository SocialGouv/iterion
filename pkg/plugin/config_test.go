package plugin

import "testing"

func TestPluginConfigEffectiveExpandAndView(t *testing.T) {
	p := &Plugin{Manifest: Manifest{
		Name: "demo",
		Config: []ConfigField{
			{Key: "max_depth", Type: "int", Default: "3"},
			{Key: "api_key", Type: "secret"},
			{Key: "mode", Type: "enum", Options: []string{"on", "ultra"}, Default: "on"},
		},
	}}
	r := &Registry{
		home:    t.TempDir(),
		plugins: []*Plugin{p},
		state:   map[string]bool{},
		config:  map[string]map[string]string{},
	}

	// Defaults apply before any override; a secret with no default is absent.
	eff := r.EffectiveConfig("demo")
	if eff["max_depth"] != "3" || eff["mode"] != "on" {
		t.Fatalf("defaults not applied: %v", eff)
	}
	if _, ok := eff["api_key"]; ok {
		t.Fatalf("secret with no default should be absent: %v", eff)
	}

	// Persist overrides and reload from disk to prove round-tripping.
	if err := r.SetConfig("demo", map[string]string{"max_depth": "7", "api_key": "sk-xyz", "blank": ""}); err != nil {
		t.Fatal(err)
	}
	if err := r.loadState(); err != nil {
		t.Fatal(err)
	}
	eff = r.EffectiveConfig("demo")
	if eff["max_depth"] != "7" || eff["api_key"] != "sk-xyz" {
		t.Fatalf("overrides not persisted: %v", eff)
	}
	if _, ok := eff["blank"]; ok {
		t.Fatalf("empty value should be dropped, not stored: %v", eff)
	}

	// {{config.<key>}} expands into commands/env using effective values.
	exp := r.ExpandContextFor("demo", "/ws")
	if got := exp.Expand("--depth {{config.max_depth}} --key {{config.api_key}}"); got != "--depth 7 --key sk-xyz" {
		t.Fatalf("expand: %q", got)
	}

	// The view never leaks a secret value, but reports it as set; non-secret
	// values are exposed.
	v, ok := r.ViewFor("demo")
	if !ok {
		t.Fatal("view missing")
	}
	if _, leaked := v.ConfigValues["api_key"]; leaked {
		t.Fatalf("secret leaked into config_values: %v", v.ConfigValues)
	}
	if v.ConfigValues["max_depth"] != "7" {
		t.Fatalf("non-secret value missing from view: %v", v.ConfigValues)
	}
	if len(v.ConfigSecretSet) != 1 || v.ConfigSecretSet[0] != "api_key" {
		t.Fatalf("api_key not reported as set: %v", v.ConfigSecretSet)
	}
}

func TestParseManifestConfig(t *testing.T) {
	ok := []byte(`
name: demo
contributes:
  lifecycle:
    index: "echo {{config.mode}}"
config:
  - key: mode
    type: enum
    options: [on, ultra]
    default: on
`)
	m, err := ParseManifest(ok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Config) != 1 || m.Config[0].Key != "mode" {
		t.Fatalf("config not parsed: %+v", m.Config)
	}

	bad := []byte(`
name: demo
contributes:
  skills: [a.md]
config:
  - key: m
    type: enum
`)
	if _, err := ParseManifest(bad); err == nil {
		t.Fatal("expected error: enum field without options")
	}
}
