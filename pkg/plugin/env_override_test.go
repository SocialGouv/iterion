package plugin

import "testing"

// newTestRegistry builds an in-memory registry over the given plugins with
// empty operator state/config (the env-override tests set env, not state).
func newTestRegistry(t *testing.T, plugins ...*Plugin) *Registry {
	return &Registry{
		home:    t.TempDir(),
		plugins: plugins,
		state:   map[string]bool{},
		config:  map[string]map[string]string{},
	}
}

func TestResolveEnabled_EnvOverrides(t *testing.T) {
	mk := func(name string, def bool) *Plugin {
		return &Plugin{Manifest: Manifest{Name: name, DefaultEnabled: def}}
	}

	t.Run("enable a default-disabled plugin", func(t *testing.T) {
		r := newTestRegistry(t, mk("firecrawl", false))
		t.Setenv("ITERION_PLUGINS_ENABLE", "firecrawl")
		t.Setenv("ITERION_PLUGINS_DISABLE", "")
		r.resolveEnabled()
		if !r.IsEnabled("firecrawl") {
			t.Fatal("ITERION_PLUGINS_ENABLE did not enable firecrawl")
		}
	})

	t.Run("disable a default-enabled plugin", func(t *testing.T) {
		r := newTestRegistry(t, mk("rtk", true))
		t.Setenv("ITERION_PLUGINS_ENABLE", "")
		t.Setenv("ITERION_PLUGINS_DISABLE", "rtk")
		r.resolveEnabled()
		if r.IsEnabled("rtk") {
			t.Fatal("ITERION_PLUGINS_DISABLE did not disable rtk")
		}
	})

	t.Run("disable wins over enable", func(t *testing.T) {
		r := newTestRegistry(t, mk("x", false))
		t.Setenv("ITERION_PLUGINS_ENABLE", "x")
		t.Setenv("ITERION_PLUGINS_DISABLE", "x")
		r.resolveEnabled()
		if r.IsEnabled("x") {
			t.Fatal("disable should win over enable")
		}
	})

	t.Run("comma and space separated", func(t *testing.T) {
		r := newTestRegistry(t, mk("a", false), mk("b", false), mk("c", false))
		t.Setenv("ITERION_PLUGINS_ENABLE", "a, b c")
		t.Setenv("ITERION_PLUGINS_DISABLE", "")
		r.resolveEnabled()
		for _, n := range []string{"a", "b", "c"} {
			if !r.IsEnabled(n) {
				t.Errorf("%s not enabled from list", n)
			}
		}
	})
}

func TestEffectiveConfig_EnvOverride(t *testing.T) {
	p := &Plugin{Manifest: Manifest{
		Name: "firecrawl",
		Config: []ConfigField{
			{Key: "api_url", Default: ""},
			{Key: "api_key", Type: "secret", Default: "self-hosted"},
		},
	}}

	t.Run("env overrides default", func(t *testing.T) {
		r := newTestRegistry(t, p)
		t.Setenv("ITERION_PLUGIN_FIRECRAWL_API_URL", "http://iterion-firecrawl:3002")
		eff := r.EffectiveConfig("firecrawl")
		if eff["api_url"] != "http://iterion-firecrawl:3002" {
			t.Fatalf("env override not applied: %v", eff)
		}
	})

	t.Run("env overrides stored operator value", func(t *testing.T) {
		r := newTestRegistry(t, p)
		r.config["firecrawl"] = map[string]string{"api_key": "stored-key"}
		t.Setenv("ITERION_PLUGIN_FIRECRAWL_API_KEY", "env-key")
		eff := r.EffectiveConfig("firecrawl")
		if eff["api_key"] != "env-key" {
			t.Fatalf("env should win over stored value: %v", eff)
		}
	})

	t.Run("no env → default/stored intact", func(t *testing.T) {
		r := newTestRegistry(t, p)
		eff := r.EffectiveConfig("firecrawl")
		if eff["api_key"] != "self-hosted" {
			t.Fatalf("default lost without env: %v", eff)
		}
	})
}
