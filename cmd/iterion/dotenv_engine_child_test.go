package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The `.env` auto-load is the CLI's convenience and therefore the human
// invocation's. A process the engine spawned against a workspace already
// holds the environment the engine handed it; reading the repository's `.env`
// on top is how a tree under review fills in the names nobody set on purpose
// — the provider endpoints a credential is spent against first among them
// (#2844). These cases drive shouldLoadWorkspaceDotEnv, the whole decision
// main() makes, and the loader itself.

// The probe variable has no routing shape: it is what the loader plants
// once it decides this invocation gets the convenience at all. The routing
// name beside it is refused whatever the decision — a dotenv file does not
// choose where a connection goes, the operator's own invocation included.
const routingVar = "DOTENV_CHILD_ROUTING_ENDPOINT"

const loadProbeVar = "DOTENV_CHILD_LOAD_PROBE"

// dotEnvWorkspace is a directory carrying a `.env` that plants a routing
// variable, with the variable unset and restored for the rest of the binary
// (the loader PLANTS it with os.Setenv, which no t.Setenv undoes).
func dotEnvWorkspace(t *testing.T) string {
	t.Helper()
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"),
		[]byte(routingVar+"=https://repo.example/v1\n"+
			loadProbeVar+"=https://repo.example/probe\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Chdir(dir)
	for _, name := range []string{routingVar, loadProbeVar} {
		restoreEnv(t, name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	return dir
}

// loadAsMainWould runs the decision and the loader exactly as main() chains
// them — the permission-hook / contracts branch first, which gates error
// tracking too, then the dotenv decision — and reports the routing variable's
// effective value afterwards. The chain, not one predicate: a skip that only
// the other branch performs must not read as this one's.
func loadAsMainWould(t *testing.T, args ...string) string {
	t.Helper()
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = args
	if !isPermissionHookInvocation() && !isContractsInvocation() {
		if shouldLoadWorkspaceDotEnv() {
			loadDotEnvFromCwd()
		}
	}
	return os.Getenv(loadProbeVar)
}

// The marker the engine writes on its children, and the control that proves
// the fixture is live: without it, the same invocation in the same directory
// still gets the convenience.
func TestEngineChildDoesNotLoadTheWorkspaceDotEnv(t *testing.T) {
	dotEnvWorkspace(t)
	t.Setenv(envtrust.EnvEngineChild, "1")

	if got := loadAsMainWould(t, "iterion", "run", "probe.bot"); got != "" {
		t.Fatalf("%s = %q, want unset — the engine's child read the repository's .env", routingVar, got)
	}
}

func TestAHumanCLIInvocationStillLoadsTheWorkspaceDotEnv(t *testing.T) {
	dotEnvWorkspace(t)

	if got := loadAsMainWould(t, "iterion", "run", "probe.bot"); got != "https://repo.example/probe" {
		t.Fatalf("%s = %q, want the .env value — the CLI convenience is gone, and every skip above is unobservable", loadProbeVar, got)
	}
	if got := os.Getenv(routingVar); got != "" {
		t.Fatalf("%s = %q, want unset — a dotenv file does not choose where a connection goes, the operator's own invocation included", routingVar, got)
	}
}

// The named escape hatch (philosophy §1): an operator who wants engine
// children to honour the workspace `.env` says so in the environment they
// launch from.
func TestTheOperatorCanForceTheDotEnvLoadForAnEngineChild(t *testing.T) {
	dotEnvWorkspace(t)
	t.Setenv(envtrust.EnvEngineChild, "1")
	t.Setenv(envtrust.EnvDotEnvForce, "1")

	if got := loadAsMainWould(t, "iterion", "run", "probe.bot"); got != "https://repo.example/probe" {
		t.Fatalf("%s = %q, want the .env value — %s is a bound with no override", loadProbeVar, got, envtrust.EnvEngineChild)
	}
	if got := os.Getenv(routingVar); got != "" {
		t.Fatalf("%s = %q, want unset — the force hatch reopens the convenience, never the routing refusal", routingVar, got)
	}
}

// And the hatch is the OPERATOR's: a `.env` that planted it in an ancestor
// cannot vote itself back in, or the skip would be the hole with one extra
// step.
func TestAPlantedForceFlagDoesNotReopenTheWorkspaceDotEnv(t *testing.T) {
	dotEnvWorkspace(t)
	t.Setenv(envtrust.EnvEngineChild, "1")
	t.Setenv(envtrust.EnvDotEnvForce, "1")
	// What an ancestor exports after its own `.env` planted the name.
	t.Setenv(envtrust.EnvPlantedNames, envtrust.EnvDotEnvForce)
	envtrust.ResetForTest()

	if got := loadAsMainWould(t, "iterion", "run", "probe.bot"); got != "" {
		t.Fatalf("%s = %q, want unset — a repository's .env granted itself the escape hatch", routingVar, got)
	}
}

// The hidden `__`-prefixed subcommands are re-entrant by construction: the
// engine, or an agent CLI the engine configured, spawns them with cwd = the
// workspace. Two of them re-execute on every tool call of a session, where
// the `.env` walk is also a per-call cost on a file the gated agent writes
// (R6fa6d2). No marker needed — argv says it.
func TestInternalSubcommandsDoNotLoadTheWorkspaceDotEnv(t *testing.T) {
	for _, argv := range [][]string{
		{"iterion", "__claw-runner"},
		{"iterion", "__scan-shards", "--mode=cloud"},
		{"iterion", "__mcp-runs"},
		{"iterion", "__mcp-board"},
		{"iterion", "__mcp-ask-user"},
		{"iterion", "__mcp-control"},
		{"iterion", "__claude-hook-drain"},
		{"iterion", "__permission-hook", "--backend", "kimi"},
		{"iterion", "--json", "__mcp-runs"},
		// Not `__`-prefixed, and skipped by the branch above since #498:
		// in the chain, as main() runs it.
		{"iterion", "contracts"},
	} {
		t.Run(argv[len(argv)-1], func(t *testing.T) {
			dotEnvWorkspace(t)
			if got := loadAsMainWould(t, argv...); got != "" {
				t.Fatalf("%v: %s = %q, want unset", argv, routingVar, got)
			}
		})
	}
}

// The decision is argv + environment and nothing else, so the public
// invocations keep the convenience and the three skips each stand on their
// own reason. Driven as a table over the real predicate: a skip added by
// widening the `__` class to a public command would show up here.
func TestShouldLoadWorkspaceDotEnv(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		child bool
		force bool
		want  bool
	}{
		{name: "a bare invocation", args: []string{"iterion"}, want: true},
		{name: "run", args: []string{"iterion", "run", "x.bot"}, want: true},
		{name: "a flag before the subcommand", args: []string{"iterion", "--json", "runs", "list"}, want: true},
		{name: "studio alone", args: []string{"iterion", "studio"}, want: true},
		{name: "mcp, the operator's own server", args: []string{"iterion", "mcp"}, want: true},
		{name: "a bot file after --", args: []string{"iterion", "run", "--", "__weird.bot"}, want: true},
		{name: "studio --workspace", args: []string{"iterion", "studio", "--workspace"}, want: false},
		{name: "studio --workspace, forced", args: []string{"iterion", "studio", "--workspace"}, force: true, want: false},
		{name: "a hidden subcommand", args: []string{"iterion", "__claw-runner"}, want: false},
		{name: "a hidden subcommand, forced", args: []string{"iterion", "__claw-runner"}, force: true, want: true},
		{name: "an engine child", args: []string{"iterion", "run", "x.bot"}, child: true, want: false},
		{name: "an engine child, forced", args: []string{"iterion", "run", "x.bot"}, child: true, force: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envtrust.ResetForTest()
			t.Cleanup(envtrust.ResetForTest)
			t.Setenv(envtrust.EnvPlantedNames, "")
			orig := os.Args
			t.Cleanup(func() { os.Args = orig })
			os.Args = tc.args
			t.Setenv(envtrust.EnvEngineChild, "")
			if tc.child {
				t.Setenv(envtrust.EnvEngineChild, "1")
			}
			t.Setenv(envtrust.EnvDotEnvForce, "")
			if tc.force {
				t.Setenv(envtrust.EnvDotEnvForce, "true")
			}
			if got := shouldLoadWorkspaceDotEnv(); got != tc.want {
				t.Errorf("args=%v child=%v force=%v: got %v, want %v", tc.args, tc.child, tc.force, got, tc.want)
			}
		})
	}
}

// firstPositionalArg is what both argv predicates read; the permission hook's
// own detector is expressed in terms of it, so neither can drift from the
// other.
func TestFirstPositionalArg(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{args: []string{"iterion"}, want: ""},
		{args: []string{"iterion", "run", "x.bot"}, want: "run"},
		{args: []string{"iterion", "--json", "__mcp-runs"}, want: "__mcp-runs"},
		// A flag VALUE is the first non-flag argument — the shape the
		// permission-hook detector has always read. Harmless in both
		// predicates: cobra wants the subcommand before its own flags, and a
		// value that happens to be `__`-shaped only costs that call the `.env`
		// convenience.
		{args: []string{"iterion", "-v", "--store-dir", "/tmp/s", "runs"}, want: "/tmp/s"},
		{args: []string{"iterion", "--", "run"}, want: ""},
		{args: []string{"iterion", "--json"}, want: ""},
	}
	for _, tc := range cases {
		orig := os.Args
		os.Args = tc.args
		got := firstPositionalArg()
		os.Args = orig
		if got != tc.want {
			t.Errorf("args=%v got %q, want %q", tc.args, got, tc.want)
		}
	}
}
