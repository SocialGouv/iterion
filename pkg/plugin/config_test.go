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

func TestEnabledRewriterSpecsExpandsConfig(t *testing.T) {
	p := &Plugin{
		Enabled: true,
		Manifest: Manifest{
			Name:   "rw",
			Config: []ConfigField{{Key: "level", Type: "enum", Options: []string{"on", "ultra"}, Default: "on"}},
			Contributes: Contributes{Rewriters: []RewriterSpec{{
				ID: "rw",
				Invoke: InvokeSpec{
					Argv: []string{"--level", "{{config.level}}", "{{command}}"},
					Env:  map[string]string{"RW_LEVEL": "{{config.level}}"},
				},
			}}},
		},
	}
	r := &Registry{home: t.TempDir(), plugins: []*Plugin{p}, state: map[string]bool{}, config: map[string]map[string]string{}}
	if err := r.SetConfig("rw", map[string]string{"level": "ultra"}); err != nil {
		t.Fatal(err)
	}
	specs := r.EnabledRewriterSpecs()
	if len(specs) != 1 {
		t.Fatalf("want 1 spec, got %d", len(specs))
	}
	s := specs[0]
	if s.Invoke.Env["RW_LEVEL"] != "ultra" {
		t.Fatalf("env not config-expanded: %v", s.Invoke.Env)
	}
	if s.Invoke.Argv[1] != "ultra" || s.Invoke.Argv[2] != "{{command}}" {
		t.Fatalf("argv expansion wrong ({{command}} must survive): %v", s.Invoke.Argv)
	}
	// The registry's manifest must be untouched (we expand a copy).
	if p.Manifest.Contributes.Rewriters[0].Invoke.Env["RW_LEVEL"] != "{{config.level}}" {
		t.Fatalf("manifest mutated: %v", p.Manifest.Contributes.Rewriters[0].Invoke.Env)
	}
}

func TestApplyConfigSecretKeepAndDeclaredOnly(t *testing.T) {
	p := &Plugin{Manifest: Manifest{Name: "x", Config: []ConfigField{
		{Key: "api_key", Type: "secret"},
		{Key: "depth", Type: "int", Default: "1"},
	}}}
	r := &Registry{home: t.TempDir(), plugins: []*Plugin{p}, state: map[string]bool{}, config: map[string]map[string]string{}}

	if err := r.ApplyConfig("x", map[string]string{"api_key": "sk-1", "depth": "5", "unknown": "z"}); err != nil {
		t.Fatal(err)
	}
	if r.EffectiveConfig("x")["api_key"] != "sk-1" || r.EffectiveConfig("x")["depth"] != "5" {
		t.Fatalf("apply failed: %v", r.EffectiveConfig("x"))
	}
	if _, ok := r.StoredConfig("x")["unknown"]; ok {
		t.Fatal("undeclared field should be rejected")
	}

	// A blank secret keeps the prior value; a non-secret updates.
	if err := r.ApplyConfig("x", map[string]string{"api_key": "", "depth": "9"}); err != nil {
		t.Fatal(err)
	}
	eff := r.EffectiveConfig("x")
	if eff["api_key"] != "sk-1" {
		t.Fatalf("blank secret should keep prior: %v", eff)
	}
	if eff["depth"] != "9" {
		t.Fatalf("non-secret not updated: %v", eff)
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
