package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/plugin"
	"github.com/SocialGouv/iterion/pkg/store"
)

// iterion auto-loads the nearest `.env` walking up from the working
// directory, filling in every variable the operator left unset. That is a
// convenience for API keys next to a project — and a hole under the one
// question the sandbox boundary rests on: ITERION_HOME selects where plugins
// are installed, an installed plugin can enable itself (`default_enabled`),
// and an enabled plugin's MCP servers are the operator's, which is precisely
// the origin the launcher agrees to start beside itself while a sandbox is
// active.
//
// A repository carrying a `.env` and a plugin manifest would therefore have
// classified its own code as the operator's. The `.env` still selects the
// home — nothing about loading changes — but the ANSWER to "is this the
// operator's" reads the environment as inherited, before any file in the
// tree under review could speak.
func TestAProjectDotenvCannotManufactureAnOperatorPlugin(t *testing.T) {
	operatorHome := t.TempDir()
	repo := t.TempDir()
	repoHome := filepath.Join(repo, ".iterion-home")
	writePlugin(t, repoHome, "repo-planted")
	writePlugin(t, operatorHome, "operator-installed")

	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte("ITERION_HOME="+repoHome+"\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// The operator set nothing, so the repository's `.env` is what decides
	// where plugins are read from.
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("HOME", operatorHome)
	if err := os.Unsetenv("ITERION_HOME"); err != nil {
		t.Fatalf("unset: %v", err)
	}

	t.Chdir(repo)
	loadDotEnvFromCwd()

	// Unchanged behaviour: the `.env` still selects the home, and everything
	// that READS it follows.
	if got := os.Getenv("ITERION_HOME"); got != repoHome {
		t.Fatalf("the .env must still select the home: %q", got)
	}
	if got := store.GlobalIterionDataDir(); got != repoHome {
		t.Fatalf("the data dir follows the live value: %q", got)
	}
	// What changed: the same value no longer speaks for the operator.
	if got := store.InheritedIterionDataDir(); got != filepath.Join(operatorHome, store.StoreDirName) {
		t.Fatalf("the inherited home must ignore the planted value: %q", got)
	}

	reg, err := plugin.Load()
	if err != nil {
		t.Fatalf("plugin.Load: %v", err)
	}
	planted, ok := reg.Get("repo-planted")
	if !ok {
		t.Fatal("the repo's plugin should still LOAD — only its authority is in question")
	}
	if reg.OperatorControlled(planted) {
		t.Error("a plugin read from a home a project .env selected must not carry the operator's authority: " +
			"its MCP servers would then start beside the launcher of a sandboxed run")
	}
	// And the subtler half: a BUILTIN is the operator's code, but the file
	// deciding whether it runs and what it is configured with now belongs to
	// the repository too — `<repoHome>/plugins.yaml`. The operator's own
	// binary, told by a repository where to send the run's data, is not the
	// operator's.
	for _, p := range reg.Enabled() {
		if p.Builtin && reg.OperatorControlled(p) && len(reg.EffectiveConfig(p.Name())) > 0 {
			t.Errorf("builtin %s is configured from a home the .env chose, yet counts as the operator's", p.Name())
		}
	}
}

// A manifest under the operator's OWN home keeps its authority — the point is
// to distinguish the two homes, not to distrust installed plugins.
func TestAPluginUnderTheInheritedHomeStaysOperatorControlled(t *testing.T) {
	home := t.TempDir()
	writePlugin(t, home, "operator-installed")

	reg, err := plugin.LoadFromForTest(home, home)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, ok := reg.Get("operator-installed")
	if !ok {
		t.Fatal("the plugin did not load")
	}
	if !reg.OperatorControlled(p) {
		t.Error("a plugin installed under the operator's own home is the operator's")
	}

	// Same manifest, a home nobody vouched for: no authority. The inherited
	// home being EMPTY ("the operator said nothing") is the fail-closed case.
	reg, err = plugin.LoadFromForTest(home, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, _ = reg.Get("operator-installed")
	if reg.OperatorControlled(p) {
		t.Error("with no inherited home, nothing installed can claim the operator's authority")
	}
}

func writePlugin(t *testing.T, home, name string) {
	t.Helper()
	dir := filepath.Join(home, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := "name: " + name + `
version: 1.0.0
description: test plugin
schema_version: 1
default_enabled: true
contributes:
  mcp_servers:
    - name: ` + name + `
      transport: stdio
      command: /bin/echo
`
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}
