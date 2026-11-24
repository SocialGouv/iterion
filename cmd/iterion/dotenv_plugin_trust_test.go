package main

import (
	"os"
	"path/filepath"
	"strings"
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
// classified its own code as the operator's. Two layers answer now: the
// loader REFUSES the name outright — ITERION_HOME is an operator-grade
// name (the bot-var gate denies it to a stored record, so the file a
// project holds gets no more), so the `.env` no longer selects the home at
// all; and the belt keeps answering "is this the operator's" as inherited
// for a value that managed to be planted anyway — the marker an OLDER
// binary's loader left in a child, which this test simulates by hand,
// since the current loader can no longer produce one.
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
	// Resolve the homes as the operator's binary does: under `go test` the
	// home tier is otherwise the test process's own.
	store.ResolveIterionHomeAsInProductionForTests(t)
	// t.Setenv restores what it sets; os.Unsetenv does not, and the loader
	// below PLANTS the variable for the rest of the test binary — pointing at
	// a directory t.TempDir's cleanup then deletes. Restore it by hand.
	restoreEnv(t, "ITERION_HOME")
	if err := os.Unsetenv("ITERION_HOME"); err != nil {
		t.Fatalf("unset: %v", err)
	}

	t.Chdir(repo)
	said := captureStderr(t, loadDotEnvFromCwd)

	// The loader refuses the name: the file no longer selects the home.
	if got := os.Getenv("ITERION_HOME"); got != "" {
		t.Fatalf("the .env planted ITERION_HOME=%q: the operator grade must refuse it", got)
	}
	if !strings.Contains(said, "ITERION_HOME") {
		t.Fatalf("the refusal does not name ITERION_HOME — it said: %s", said)
	}

	// The belt, for a value that managed to be planted anyway (an older
	// binary's marker in a child): the live value still selects the home,
	// and the inherited answer stays the operator's.
	if err := os.Setenv("ITERION_HOME", repoHome); err != nil {
		t.Fatalf("set: %v", err)
	}
	t.Setenv(envtrust.EnvPlantedNames, "ITERION_HOME")
	envtrust.ResetForTest()
	if got := store.GlobalIterionDataDir(); got != repoHome {
		t.Fatalf("the data dir follows the live value: %q", got)
	}
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

// restoreEnv captures a variable's current state and puts it back when the
// test ends, for the cases t.Setenv cannot cover: a variable the test UNSETS,
// and one the code under test then sets itself.
func restoreEnv(t *testing.T, name string) {
	t.Helper()
	old, had := os.LookupEnv(name)
	t.Cleanup(func() {
		if had {
			if err := os.Setenv(name, old); err != nil {
				t.Errorf("restore %s: %v", name, err)
			}
			return
		}
		if err := os.Unsetenv(name); err != nil {
			t.Errorf("unset %s: %v", name, err)
		}
	})
}

// applyDotEnv refuses a key carrying the planted-names separator: such a key
// would reach a child as TWO names, and the extra one denies the OPERATOR's
// own environment its authority downstream. No shell can export the name
// either, so the line is malformed and skipped whole.
//
// The `envtrust` backstop covers the damage; this covers the source, because
// a guard nobody mutated is not a guard.
func TestADotenvKeyCarryingTheSeparatorIsRefused(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte("ITERION_HOME,HARMLESS=1\nITERION_TEST_PLAIN=kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	restoreEnv(t, "ITERION_TEST_PLAIN")
	restoreEnv(t, "ITERION_HOME,HARMLESS")

	t.Chdir(repo)
	loadDotEnvFromCwd()

	if v, ok := os.LookupEnv("ITERION_HOME,HARMLESS"); ok {
		t.Errorf("the malformed key was set to %q; it must be skipped whole", v)
	}
	if envtrust.Planted("ITERION_HOME") {
		t.Error("the operator's own home must not read as planted: the marker would carry the extra name " +
			"into every child process")
	}
	// …and the well-formed line on the next row still applies.
	if os.Getenv("ITERION_TEST_PLAIN") != "kept" {
		t.Error("one malformed key must not cost the rest of the file")
	}
}

// On Windows a variable is the same whatever the case of its name, so a `.env`
// key is judged by the variable it really sets: the provenance marker is
// refused in lower case too, and a lower-case key reads as planted under the
// upper-case name — else `iterion_home=…` planted ITERION_HOME unseen.
func TestADotenvKeyIsJudgedByTheVariableItSetsOnWindows(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte("iterion_dotenv_planted=ITERION_HOME\niterion_test_lower=planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.UseGOOSForTest(t, "windows")
	restoreEnv(t, "iterion_dotenv_planted")
	restoreEnv(t, "iterion_test_lower")

	t.Chdir(repo)
	loadDotEnvFromCwd()

	if v, ok := os.LookupEnv("iterion_dotenv_planted"); ok {
		t.Errorf("the provenance marker spelt in lower case was set to %q; a .env must never write it", v)
	}
	if !envtrust.Planted("ITERION_TEST_LOWER") {
		t.Error("a key planted in lower case must read as planted under the upper-case name Windows sets")
	}
}

// A key no shell could export is refused — the comma's class. Outside ASCII it
// matters on Windows: how Windows folds a non-ASCII letter is no rule
// envtrust.CanonicalName can mirror, and `ıTERION_HOME` folded to ITERION_HOME
// there marked the operator's own home as planted, stripping its authority,
// while setting a variable nothing reads.
func TestADotenvKeyNoShellCouldExportIsRefused(t *testing.T) {
	keys := []string{"ıTERION_HOME", "ITERION_PLUGINſ_ENABLE"}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".env"),
		[]byte(keys[0]+"=x\n"+keys[1]+"=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.UseGOOSForTest(t, "windows")
	for _, key := range keys {
		restoreEnv(t, key)
	}

	t.Chdir(repo)
	loadDotEnvFromCwd()

	for _, name := range []string{"ITERION_HOME", "ITERION_PLUGINS_ENABLE"} {
		if envtrust.Planted(name) {
			t.Errorf("a .env key outside ASCII marked the operator's own %s as planted", name)
		}
	}
	for _, key := range keys {
		if v, ok := os.LookupEnv(key); ok {
			t.Errorf("the key %q no shell could export was set to %q; it must be refused", key, v)
		}
	}
}
