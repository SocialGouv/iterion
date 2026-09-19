package model

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// The tree-noise env rides the executor field pattern (plan review F1, #1464):
// it is appended at every host tool spawn UNCONDITIONALLY — never through run
// provisioning — because provisionHostDevbox early-returns on a workspace
// without a devbox.json, and "no devbox" is the common run. A scope gate that
// reads ITERION_TREE_NOISE must not depend on what the workspace happens to
// pin. Both tool-node paths the host executor builds carry it: the shell
// command and the script interpreter.
func TestToolNodeCommandsCarryTheTreeNoiseEnvWithoutAnyProvisioning(t *testing.T) {
	e := &ClawExecutor{} // no sandbox, no artifact dir, no runExtraEnv, no devbox

	cmd := e.toolNodeCommand(context.Background(), "git status --porcelain -- ':/'", nil)
	got := envValue(cmd.Env, "ITERION_TREE_NOISE")
	if got != treenoise.EnvValue() {
		t.Fatalf("shell command ITERION_TREE_NOISE = %q, want %q", got, treenoise.EnvValue())
	}

	sc := e.toolNodeScriptCommand(context.Background(), "python3", "scope_check.py")
	got = envValue(sc.Env, "ITERION_TREE_NOISE")
	if got != treenoise.EnvValue() {
		t.Fatalf("script command ITERION_TREE_NOISE = %q, want %q", got, treenoise.EnvValue())
	}
}

// The env rides an inherited environment: the child still sees the parent's
// variables (a bot's PATH, its credentials), with the noise list appended.
func TestToolNodeCommandsKeepTheInheritedEnvironment(t *testing.T) {
	t.Setenv("ITERION_NOISE_CANARY", "here")
	e := &ClawExecutor{}

	cmd := e.toolNodeCommand(context.Background(), "true", nil)
	if envValue(cmd.Env, "ITERION_NOISE_CANARY") != "here" {
		t.Fatalf("the inherited environment was dropped: ITERION_NOISE_CANARY missing from %q", cmd.Env)
	}
	if envValue(cmd.Env, "ITERION_TREE_NOISE") == "" {
		t.Fatalf("the noise list is missing from %q", cmd.Env)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

// envContainsEnv mirrors what the executor does: os.Environ() first, the
// noise list appended. Used to assert the appended slice is not built on a
// nil base (which would drop the inherited environment silently).
func TestToolNodeEnvBaseIsTheInheritedEnvironment(t *testing.T) {
	e := &ClawExecutor{}
	cmd := e.toolNodeCommand(context.Background(), "true", nil)
	parent := os.Environ()
	if len(cmd.Env) < len(parent) {
		t.Fatalf("cmd.Env has %d entries, want at least the parent's %d", len(cmd.Env), len(parent))
	}
}

// The agent path carries the variable too: an agent node's own Bash (the
// claw builtin) receives Task.ExtraEnv, so a scope check an AGENT runs
// inline sees the same list a tool script does — on an unsandboxed run with
// no devbox.json, where provisioning never fires (plan review F3).
func TestBuildTaskCarriesTheTreeNoiseEnv(t *testing.T) {
	src, err := os.ReadFile("executor_build_task.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "treeNoiseEnvAppend(nil)") {
		t.Fatalf("executor_build_task.go no longer wires the tree-noise env into Task.ExtraEnv — an agent's own bash loses the list on an unsandboxed run")
	}
}

// The append yields to every prior claim on the variable (verdicts 2-3):
// the node's env map, the run's env, and the operator's own exported
// environment each win — nothing is appended, and the child inherits their
// value unchanged.
func TestTreeNoiseEnvAppendYieldsToEveryPriorClaim(t *testing.T) {
	// An operator who exported the variable themselves must not find the
	// suite red on their own shell: skip this executor's canonical case
	// (and the operator-claim case below) when the ambient environment
	// already carries it.
	if _, exported := os.LookupEnv(treenoise.TreeNoiseEnvVar); exported {
		t.Skipf("%s is set in the ambient environment — the canonical-entry and operator-claim cases need it absent", treenoise.TreeNoiseEnvVar)
	}

	// No prior claim: the canonical entry. FIRST, before any t.Setenv —
	// which would otherwise shadow the rest of the test.
	fresh := &ClawExecutor{}
	got := fresh.treeNoiseEnvAppend(nil)
	if len(got) != 1 || !strings.HasPrefix(got[0], "ITERION_TREE_NOISE=:(exclude,top).claude ") {
		t.Fatalf("treeNoiseEnvAppend without any prior claim = %q, want exactly the canonical entry", got)
	}

	// The run's env claims it.
	e := &ClawExecutor{}
	e.SetRunExtraEnv([]string{"ITERION_TREE_NOISE=':(exclude,top)run'"})
	if got := e.treeNoiseEnvAppend(map[string]string{}); got != nil {
		t.Fatalf("append over a run-set variable = %q, want nil (the run's value wins)", got)
	}
	// The node's env map claims it.
	if got := e.treeNoiseEnvAppend(map[string]string{treenoise.TreeNoiseEnvVar: "':(exclude,top)node'"}); got != nil {
		t.Fatalf("append over a node-set variable = %q, want nil (the node's value wins)", got)
	}
	// The operator's own exported environment claims it (t.Setenv holds to
	// the end of the test, so this case stays last) — on a FRESH executor,
	// so the run-set value from the earlier case cannot mask the claim.
	t.Setenv(treenoise.TreeNoiseEnvVar, "':(exclude,top)operator'")
	if got := fresh.treeNoiseEnvAppend(nil); got != nil {
		t.Fatalf("append over an operator-exported variable = %q, want nil (the operator's value wins)", got)
	}
}

// One run, one list, whichever surface gates the tree (verdict 2,
// R05b122): when the run's own env or the node's env map already carries
// ITERION_TREE_NOISE, the host tool commands keep that value instead of
// appending the engine's entry after it — the same step-aside the sandbox
// seed and the agent task do.
func TestToolNodeCommandsStepAsideForARunOrNodeValue(t *testing.T) {
	e := &ClawExecutor{}
	e.SetRunExtraEnv([]string{"ITERION_TREE_NOISE=':(exclude,top)vendor'"})

	cmd := e.toolNodeCommand(context.Background(), "true", nil)
	entries := envEntries(cmd.Env, "ITERION_TREE_NOISE")
	if len(entries) != 1 || entries[0] != "':(exclude,top)vendor'" {
		t.Fatalf("shell command ITERION_TREE_NOISE entries = %q, want exactly the run's value", entries)
	}

	nodeEnv := map[string]string{treenoise.TreeNoiseEnvVar: "':(exclude,top)node'"}
	cmd = e.toolNodeCommand(context.Background(), "true", nodeEnv)
	entries = envEntries(cmd.Env, "ITERION_TREE_NOISE")
	// The node's env map is appended after the run's env, and the CHILD's
	// env dedups last-wins (bash and every interpreter here) — what
	// matters is that the most specific value is the one the child sees.
	if len(entries) == 0 || entries[len(entries)-1] != "':(exclude,top)node'" {
		t.Fatalf("shell command with a node-env value = %q, want the node's value last", entries)
	}

	sc := e.toolNodeScriptCommand(context.Background(), "python3", "scope_check.py")
	entries = envEntries(sc.Env, "ITERION_TREE_NOISE")
	if len(entries) != 1 || entries[0] != "':(exclude,top)vendor'" {
		t.Fatalf("script command ITERION_TREE_NOISE entries = %q, want exactly the run's value", entries)
	}
}

func envEntries(env []string, key string) []string {
	prefix := key + "="
	out := []string{}
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			out = append(out, strings.TrimPrefix(entry, prefix))
		}
	}
	return out
}
