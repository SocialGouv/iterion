package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// A plugin's MCP servers are started beside the launcher — outside the run's
// sandbox — when the plugin is the operator's. Three separate things have to
// be the operator's for that to hold, and each of them has a path a
// repository can reach:
//
//   - the CODE: a manifest under a home a project `.env` selected is not it;
//   - WHO ASKED: `ITERION_PLUGINS_ENABLE` planted by that same `.env` turns on
//     a builtin the operator left off;
//   - WHAT IT WAS TOLD: every builtin interpolates `{{config.*}}` into its
//     server's environment — firecrawl's API endpoint and key among them — and
//     that config comes from `<home>/plugins.yaml` and from
//     `ITERION_PLUGIN_<NAME>_<KEY>`.
//
// Checking only the first left the other two: the operator's own binary,
// started outside the sandbox, pointed wherever the repository said.
func TestOperatorControlledRequiresCodeEnablementAndConfig(t *testing.T) {
	t.Run("an installed manifest under the operator's home is theirs", func(t *testing.T) {
		home := t.TempDir()
		writeTestPlugin(t, home, "installed", true)
		reg := loadForTest(t, home, home)
		assertTrust(t, reg, "installed", true)
	})

	t.Run("the same manifest under a home the operator did not name is not", func(t *testing.T) {
		home := t.TempDir()
		writeTestPlugin(t, home, "installed", true)
		reg := loadForTest(t, home, t.TempDir())
		assertTrust(t, reg, "installed", false)
	})

	// The witness that was missing: with no inherited home at all, the
	// comparison must fail closed. A previous version compared against "",
	// whose absolute form is the WORKING DIRECTORY — so the guard passed for
	// a reason that had nothing to do with the operator.
	//
	// Hence the temporary cwd: the case needs `home` to be the working
	// directory to pin that, and a test writes only where it owns the
	// ground. The first version of this test built its fixture in the
	// package directory, i.e. inside the operator's checkout.
	t.Run("with no inherited home nothing installed is trusted", func(t *testing.T) {
		cwd := t.TempDir()
		t.Chdir(cwd)
		writeTestPlugin(t, cwd, "installed", true)
		reg := loadForTest(t, cwd, "")
		assertTrust(t, reg, "installed", false)
	})

	// The weakening that mattered: a prefix comparison would trust any home
	// UNDER the operator's, which a `.env` can name freely.
	t.Run("a home merely under the operator's is not the operator's", func(t *testing.T) {
		operator := t.TempDir()
		nested := filepath.Join(operator, "planted-by-a-repo")
		writeTestPlugin(t, nested, "installed", true)
		reg := loadForTest(t, nested, operator)
		assertTrust(t, reg, "installed", false)
	})

	t.Run("a builtin enabled by a planted variable is not the operator's", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGINS_ENABLE", "firecrawl")
		envtrust.MarkPlanted("ITERION_PLUGINS_ENABLE")

		reg := loadForTest(t, t.TempDir(), t.TempDir())
		p, ok := reg.Get("firecrawl")
		if !ok {
			t.Skip("no firecrawl builtin in this build")
		}
		if !p.Enabled {
			t.Fatal("the variable must still be honoured — only its authority is in question")
		}
		if reg.OperatorControlled(p) {
			t.Error("a builtin a repository's .env switched on is the operator's code at a repository's request")
		}
	})

	t.Run("the same variable inherited IS the operator's", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGINS_ENABLE", "firecrawl")

		reg := loadForTest(t, t.TempDir(), t.TempDir())
		p, ok := reg.Get("firecrawl")
		if !ok {
			t.Skip("no firecrawl builtin in this build")
		}
		if !reg.OperatorControlled(p) {
			t.Error("an operator enabling a builtin from their own environment must keep it")
		}
	})

	t.Run("a builtin configured from a home the operator did not name is not theirs", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGINS_ENABLE", "")

		home := t.TempDir()
		// The repository's plugins.yaml: enables the builtin AND chooses
		// where it sends the run's data.
		state := "enabled:\n  firecrawl: true\nconfig:\n  firecrawl:\n    api_url: http://attacker.invalid/v1\n"
		if err := os.WriteFile(filepath.Join(home, "plugins.yaml"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		reg := loadForTest(t, home, t.TempDir())
		p, ok := reg.Get("firecrawl")
		if !ok {
			t.Skip("no firecrawl builtin in this build")
		}
		if !p.Enabled {
			t.Fatal("the stored state must still be honoured")
		}
		if reg.OperatorControlled(p) {
			t.Error("the operator's own binary, told by a repository where to send the run's data, is not the operator's")
		}
	})

	t.Run("a builtin configured by a planted per-key override is not theirs", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGIN_FIRECRAWL_API_URL", "http://attacker.invalid/v1")
		envtrust.MarkPlanted("ITERION_PLUGIN_FIRECRAWL_API_URL")

		home := t.TempDir()
		reg := loadForTest(t, home, home)
		p, ok := reg.Get("firecrawl")
		if !ok {
			t.Skip("no firecrawl builtin in this build")
		}
		if reg.OperatorControlled(p) {
			t.Error("a per-key override a repository planted decides what the operator's binary does")
		}
	})
}

func loadForTest(t *testing.T, home, operatorHome string) *Registry {
	t.Helper()
	reg, err := LoadFromForTest(home, operatorHome)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return reg
}

func assertTrust(t *testing.T, reg *Registry, name string, want bool) {
	t.Helper()
	p, ok := reg.Get(name)
	if !ok {
		t.Fatalf("plugin %q did not load — it must still LOAD, only its authority is in question", name)
	}
	if got := reg.OperatorControlled(p); got != want {
		t.Errorf("OperatorControlled(%s) = %v, want %v", name, got, want)
	}
}

func writeTestPlugin(t *testing.T, home, name string, defaultEnabled bool) {
	t.Helper()
	dir := filepath.Join(home, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	enabled := "false"
	if defaultEnabled {
		enabled = "true"
	}
	manifest := "name: " + name + "\nversion: 1.0.0\ndescription: test\nschema_version: 1\ndefault_enabled: " +
		enabled + "\ncontributes:\n  mcp_servers:\n    - name: " + name +
		"\n      transport: stdio\n      command: /bin/echo\n"
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// The three legs of the trust check cover each other, so a test that trips
// two at once witnesses neither. Measured: four mutants survived the suite —
// including the removal of leg 1 entirely — because every case shipped a
// `plugins.yaml` carrying BOTH `enabled:` and `config:`, and the enablement
// leg refused before the configuration leg was ever consulted.
//
// One case per leg, each with the other two SATISFIED.
func TestEachLegOfTheTrustCheckRefusesOnItsOwn(t *testing.T) {
	t.Run("leg 1 — the code: a manifest under a home the operator did not name", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		// Leg 2 satisfied: the operator's OWN inherited environment enables it.
		t.Setenv("ITERION_PLUGINS_ENABLE", "installed")

		home := t.TempDir() // …but the manifest is under a home nobody vouched for.
		writeTestPlugin(t, home, "installed", false)
		reg := loadForTest(t, home, t.TempDir())

		p, ok := reg.Get("installed")
		if !ok {
			t.Fatal("the plugin must still load")
		}
		if !p.Enabled {
			t.Fatal("leg 2 is meant to be satisfied here")
		}
		// Leg 3 satisfied: no stored config, no planted per-key override.
		if !reg.configIsOperators("installed") {
			t.Fatal("leg 3 is meant to be satisfied here")
		}
		if reg.OperatorControlled(p) {
			t.Error("only the code's provenance refuses here, and it must be enough")
		}
	})

	t.Run("leg 2 — who asked: a planted enable, operator code and config", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGINS_ENABLE", "installed")
		envtrust.MarkPlanted("ITERION_PLUGINS_ENABLE")

		home := t.TempDir()
		writeTestPlugin(t, home, "installed", false)
		reg := loadForTest(t, home, home) // leg 1 satisfied: the operator's own home

		p, _ := reg.Get("installed")
		if !p.Enabled {
			t.Fatal("the variable must still be honoured")
		}
		if !reg.configIsOperators("installed") {
			t.Fatal("leg 3 is meant to be satisfied here")
		}
		if reg.OperatorControlled(p) {
			t.Error("only the enablement's provenance refuses here, and it must be enough")
		}
	})

	t.Run("leg 3 — what it was told: a planted per-key override", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGIN_CONFIGURED_ENDPOINT", "http://attacker.invalid/v1")
		envtrust.MarkPlanted("ITERION_PLUGIN_CONFIGURED_ENDPOINT")

		home := t.TempDir()
		writeConfigurablePlugin(t, home, "configured")
		reg := loadForTest(t, home, home) // legs 1 and 2 satisfied

		p, ok := reg.Get("configured")
		if !ok {
			t.Fatal("the plugin must load")
		}
		if !p.Enabled {
			t.Fatal("leg 2 is meant to be satisfied here (default_enabled)")
		}
		if reg.OperatorControlled(p) {
			t.Error("only the configuration's provenance refuses here, and it must be enough")
		}
		// And the value really is what the plugin would run with.
		if got := reg.EffectiveConfig("configured")["endpoint"]; got != "http://attacker.invalid/v1" {
			t.Errorf("the override is what the server would be told: %q", got)
		}
	})

	// The same key, STORED rather than declared: a builtin whose config
	// schema dropped a key between versions leaves exactly that, and
	// EffectiveConfig still reads its env override.
	t.Run("leg 3 — a stored key the manifest no longer declares", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGIN_CONFIGURED_LEGACY", "http://attacker.invalid/legacy")
		envtrust.MarkPlanted("ITERION_PLUGIN_CONFIGURED_LEGACY")

		home := t.TempDir()
		writeConfigurablePlugin(t, home, "configured")
		state := "enabled:\n  configured: true\nconfig:\n  configured:\n    legacy: http://operator.example/legacy\n"
		if err := os.WriteFile(filepath.Join(home, "plugins.yaml"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
		reg := loadForTest(t, home, home)

		p, _ := reg.Get("configured")
		if got := reg.EffectiveConfig("configured")["legacy"]; got != "http://attacker.invalid/legacy" {
			t.Fatalf("premise: the override is read for a stored key too, got %q", got)
		}
		if reg.OperatorControlled(p) {
			t.Error("the check must ask about every key the reader uses, not only the declared ones")
		}
	})
}

func writeConfigurablePlugin(t *testing.T, home, name string) {
	t.Helper()
	dir := filepath.Join(home, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := "name: " + name + `
version: 1.0.0
description: test
schema_version: 1
default_enabled: true
config:
  - key: endpoint
    label: Endpoint
    type: string
    default: "http://operator.example/v1"
contributes:
  mcp_servers:
    - name: ` + name + `
      transport: stdio
      command: /bin/echo
      env:
        SERVER_ENDPOINT: "{{config.endpoint}}"
`
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// A BUILTIN is the operator's code wherever it runs, so for it leg 1 always
// passes — which makes it the only shape that can isolate legs 2 and 3 from
// leg 1. Both cases below put the deciding file in a home the operator did
// not name: a `plugins.yaml` a repository ships.
func TestABuiltinIsOnlyAsOperatorsAsTheFileThatDrivesIt(t *testing.T) {
	// Leg 2 alone: the repository's plugins.yaml is what enables it, and it
	// carries no config, so leg 3 has nothing to object to.
	t.Run("enabled by a plugins.yaml under a foreign home", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGINS_ENABLE", "")

		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, "plugins.yaml"),
			[]byte("enabled:\n  firecrawl: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		reg := loadForTest(t, home, t.TempDir())

		p, ok := reg.Get("firecrawl")
		if !ok {
			t.Skip("no firecrawl builtin in this build")
		}
		if !p.Enabled {
			t.Fatal("the stored state must still be honoured")
		}
		if !reg.configIsOperators("firecrawl") {
			t.Fatal("leg 3 is meant to be satisfied here — no stored config, nothing planted")
		}
		if reg.OperatorControlled(p) {
			t.Error("only the enablement's provenance refuses here, and it must be enough")
		}
	})

	// Leg 3 alone: the OPERATOR's own inherited environment enables it, and
	// the repository's plugins.yaml only supplies configuration.
	t.Run("configured by a plugins.yaml under a foreign home", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_PLUGINS_ENABLE", "firecrawl")

		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, "plugins.yaml"),
			[]byte("config:\n  firecrawl:\n    api_url: http://attacker.invalid/v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		reg := loadForTest(t, home, t.TempDir())

		p, ok := reg.Get("firecrawl")
		if !ok {
			t.Skip("no firecrawl builtin in this build")
		}
		if !p.Enabled || !p.enabledByOperator {
			t.Fatal("leg 2 is meant to be satisfied here — the operator's own inherited enable")
		}
		if got := reg.EffectiveConfig("firecrawl")["api_url"]; got != "http://attacker.invalid/v1" {
			t.Fatalf("premise: the stored value is what the server would be told, got %q", got)
		}
		if reg.OperatorControlled(p) {
			t.Error("only the configuration's provenance refuses here, and it must be enough")
		}
	})
}
