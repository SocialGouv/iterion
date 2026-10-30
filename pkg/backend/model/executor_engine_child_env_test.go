package model

import (
	"context"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The engine declares the invocation class on every child it spawns against
// the workspace, so an iterion re-entered down there — by a bot script
// through ITERION_ENGINE_BIN, by an agent's bash tool, by the runner itself
// — does not auto-load the `.env` of the repository under review (#2844).
// One case per spawn site: a site that forgets the marker hands the tree
// under audit the provider endpoints nobody set on purpose, and nothing else
// in the suite sees it.

// The two host tool-node builders: the command and the script.
func TestHostToolNodeCommandsCarryTheEngineChildMarker(t *testing.T) {
	e := &ClawExecutor{}

	cmd := e.toolNodeCommand(context.Background(), "true", nil).cmd
	if got := envValue(cmd.Env, envtrust.EnvEngineChild); got != "1" {
		t.Errorf("shell command %s = %q, want %q", envtrust.EnvEngineChild, got, "1")
	}

	sc := e.toolNodeScriptCommand(context.Background(), []string{"python3", "-I"}, "probe.py", nil).cmd
	if got := envValue(sc.Env, envtrust.EnvEngineChild); got != "1" {
		t.Errorf("script command %s = %q, want %q", envtrust.EnvEngineChild, got, "1")
	}
}

// The sandbox-routed twins of the same two builders. The marker crosses
// whatever the engine binary resolved to: the class is a fact about the spawn.
func TestSandboxedToolNodeCommandsCarryTheEngineChildMarker(t *testing.T) {
	e := &ClawExecutor{}
	fake := &isolatedSandboxRun{}
	e.SetSandbox(fake)

	e.toolNodeCommand(context.Background(), "true", nil)
	e.toolNodeScriptCommand(context.Background(), []string{"python3", "-I"}, "probe.py", nil)
	if len(fake.envs) != 2 {
		t.Fatalf("recorded %d commands, want 2", len(fake.envs))
	}
	for i, env := range fake.envs {
		if got := env[envtrust.EnvEngineChild]; got != "1" {
			t.Errorf("sandboxed command %d: %s = %q, want %q", i, envtrust.EnvEngineChild, got, "1")
		}
	}
}

// A node's own `env:` block does not get to spell the marker away: the
// engine-owned entry is applied over the node's map, as the binary path is.
func TestToolNodeEnvCannotForgeTheEngineChildMarker(t *testing.T) {
	e := &ClawExecutor{}
	fake := &isolatedSandboxRun{}
	e.SetSandbox(fake)

	e.toolNodeCommand(context.Background(), "true", map[string]string{envtrust.EnvEngineChild: ""})
	if len(fake.envs) != 1 {
		t.Fatalf("recorded %d commands, want 1", len(fake.envs))
	}
	if got := fake.envs[0][envtrust.EnvEngineChild]; got != "1" {
		t.Errorf("%s = %q, want the engine's %q — a node body claimed its child is a human's CLI call", envtrust.EnvEngineChild, got, "1")
	}
}

// Task.ExtraEnv: what every CLI-agent spawn and the claw bash builtin apply.
// Last, so a duplicate key from the launch surface or the run's provisioning
// loses to the engine's own entry.
func TestDelegateTaskExtraEnvEndsWithTheEngineChildMarker(t *testing.T) {
	e := &ClawExecutor{}
	e.SetRunExtraEnv([]string{envtrust.EnvEngineChild + "=", "PATH=/project/bin"})

	env := e.taskExtraEnv()
	if len(env) == 0 {
		t.Fatal("the delegate task carries no env at all")
	}
	if last, want := env[len(env)-1], envtrust.EngineChildEnvEntry(); last != want {
		t.Errorf("last entry = %q, want %q — an earlier value wins in exec.Cmd, so the launch surface could unset the marker", last, want)
	}
}

// The runner crossing: `iterion __claw-runner` is this binary re-entered with
// cwd = the workspace inside the container, and the credential the engine
// resolved crosses with it.
func TestSandboxRunnerCrossingCarriesTheEngineChildMarker(t *testing.T) {
	operatorEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-test-crossing")
	box := newDeliverySandbox(t)

	env := runnerEnvOf(t, context.Background(), box, "openai/gpt-6")
	if got := env[envtrust.EnvEngineChild]; got != "1" {
		t.Errorf("runner env %s = %q, want %q — the runner would load the workspace .env over the credential that just crossed", envtrust.EnvEngineChild, got, "1")
	}
}

// The marker leads, and it is the unconditional half: the binary path is
// appended only when a resolution succeeds, and the two share one producer, so
// folding the marker into that `ok` would drop it exactly where the resolution
// fails — a host the test machine cannot be made into (the fallback walks
// fixed install paths, and a CI image carries one).
func TestEngineChildEntriesLeadWithTheMarker(t *testing.T) {
	e := &ClawExecutor{}

	entries := e.engineChildEnvEntries()
	if len(entries) == 0 || entries[0] != envtrust.EngineChildEnvEntry() {
		t.Fatalf("entries = %q, want %q first", entries, envtrust.EngineChildEnvEntry())
	}
}

// The witness the class rests on, through the real producer: a tool node's
// command inherits os.Environ(), and the entry the engine appends is what
// exec hands the child last — so the child's effective value is the
// engine's, whatever the host env said.
func TestEngineChildMarkerWinsOverTheInheritedEnvironment(t *testing.T) {
	t.Setenv(envtrust.EnvEngineChild, "")
	e := &ClawExecutor{}

	cmd := e.toolNodeCommand(context.Background(), "true", nil).cmd
	// exec.Cmd keeps the LAST duplicate; mirror that rather than trusting
	// the first match envValue returns.
	last := ""
	for _, entry := range cmd.Env {
		if len(entry) > len(envtrust.EnvEngineChild) && entry[:len(envtrust.EnvEngineChild)+1] == envtrust.EnvEngineChild+"=" {
			last = entry[len(envtrust.EnvEngineChild)+1:]
		}
	}
	if last != "1" {
		t.Errorf("effective %s = %q, want %q (host env had it empty)", envtrust.EnvEngineChild, last, "1")
	}
	if os.Getenv(envtrust.EnvEngineChild) != "" {
		t.Error("the test's own environment was mutated")
	}
}
