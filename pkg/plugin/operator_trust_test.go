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
	// whose absolute form is the working directory — so the guard passed for
	// a reason that had nothing to do with the operator.
	t.Run("with no inherited home nothing installed is trusted", func(t *testing.T) {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		writeTestPlugin(t, cwd, "installed", true)
		t.Cleanup(func() { removeTestPlugin(t, cwd, "installed") })
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

// removeTestPlugin cleans up the one case that cannot use t.TempDir: the
// working directory, which the "no inherited home" case needs because "" 's
// absolute form IS the working directory.
func removeTestPlugin(t *testing.T, home, name string) {
	t.Helper()
	dir := filepath.Join(home, "plugins", name)
	if err := os.Remove(filepath.Join(dir, ManifestFile)); err != nil && !os.IsNotExist(err) {
		t.Errorf("cleanup manifest: %v", err)
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		t.Errorf("cleanup dir: %v", err)
	}
	if err := os.Remove(filepath.Join(home, "plugins")); err != nil && !os.IsNotExist(err) {
		// Not fatal: another test's fixture may share it.
		t.Logf("cleanup plugins dir: %v", err)
	}
}
