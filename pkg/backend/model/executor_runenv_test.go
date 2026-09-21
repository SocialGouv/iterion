package model

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The engine's host devbox provisioning (pkg/runtime/devbox_host.go)
// reaches the executor through structural interfaces it type-asserts at
// run start — and SKIPS provisioning entirely when the assert fails.
// These compile-time locks keep ClawExecutor implementing both halves of
// the seam: if a method is renamed or dropped, cloud runs would silently
// lose their bot's devbox toolchain (and the engine `iterion` shim) again.
var (
	_ interface{ SetRunExtraEnv(env []string) }           = (*ClawExecutor)(nil)
	_ interface{ SetEngineExtraEnv(env []string) }        = (*ClawExecutor)(nil)
	_ interface{ GetRunExtraEnvValue(key string) string } = (*ClawExecutor)(nil)
)

// TestRunExtraEnvReachesHostToolCommands locks the consumer half of the
// run-level env seam: entries pushed via SetRunExtraEnv must land in the
// environment of host tool-node commands (both shell and script modes),
// appended after the inherited environment so they win on duplicate keys.
func TestRunExtraEnvReachesHostToolCommands(t *testing.T) {
	e := &ClawExecutor{}
	e.SetRunExtraEnv([]string{"PATH=/devbox/profile/bin:/usr/bin"})

	cmd := e.toolNodeCommand(context.Background(), "true", nil)
	if !slices.Contains(cmd.Env, "PATH=/devbox/profile/bin:/usr/bin") {
		t.Errorf("toolNodeCommand env misses the run-level PATH entry: %v", cmd.Env)
	}

	script := e.toolNodeScriptCommand(context.Background(), "sh", "x.sh")
	if !slices.Contains(script.Env, "PATH=/devbox/profile/bin:/usr/bin") {
		t.Errorf("toolNodeScriptCommand env misses the run-level PATH entry: %v", script.Env)
	}
}

func TestSetRunExtraEnvMergesAndLetsTheNewestValueWin(t *testing.T) {
	e := &ClawExecutor{}
	e.SetRunExtraEnv([]string{"PROJECT_ONLY=one", "PATH=/project/bin"})
	e.SetRunExtraEnv([]string{"PATH=/devbox/bin", "DEVBOX_ONLY=two"})
	got := map[string]string{}
	for _, entry := range e.runExtraEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["PROJECT_ONLY"] != "one" || got["DEVBOX_ONLY"] != "two" {
		t.Fatalf("merged environment = %#v", got)
	}
	if got["PATH"] != "/devbox/bin" {
		t.Fatalf("PATH = %q, want newest value", got["PATH"])
	}
}

// TestEngineExtraEnvComposesOnTheLaunchLayerAndNeverFeedsBackIntoIt pins
// the two-layer contract the engine's PATH composition relies on: what
// the engine pushes (SetEngineExtraEnv) reaches every host-spawned
// command and wins over the launch surface's layer on a duplicate key,
// while GetRunExtraEnvValue keeps answering the LAUNCH SURFACE's value —
// so a second composition on the same executor starts from the same base
// instead of prepending to the first composition.
//
// Mutations seen red: SetEngineExtraEnv merging into runExtraEnv (the
// getter then returns the composed PATH); processExtraEnv returning the
// launch layer alone (the engine's PATH never reaches a command);
// processExtraEnv merging the layers in the other order (a later launch
// push clobbers the engine's PATH).
func TestEngineExtraEnvComposesOnTheLaunchLayerAndNeverFeedsBackIntoIt(t *testing.T) {
	e := &ClawExecutor{}
	e.SetRunExtraEnv([]string{"PATH=/project/bin", "PROJECT_ONLY=one"})

	// Two provisionings of the same executor, as two runs sharing it
	// would do: each composes on the launch surface's PATH.
	for _, shim := range []string{"/shim-first", "/shim-second"} {
		base := e.GetRunExtraEnvValue("PATH")
		if base != "/project/bin" {
			t.Fatalf("launch-surface PATH read back as %q before composing %s, want /project/bin — the engine layer leaked into the getter", base, shim)
		}
		e.SetEngineExtraEnv([]string{"PATH=" + shim + ":" + base})
	}
	if got := e.GetRunExtraEnvValue("PATH"); got != "/project/bin" {
		t.Fatalf("launch-surface PATH = %q after two engine pushes, want /project/bin untouched", got)
	}

	cmd := e.toolNodeCommand(context.Background(), "true", nil)
	if !slices.Contains(cmd.Env, "PATH=/shim-second:/project/bin") {
		t.Errorf("toolNodeCommand env misses the engine's composed PATH: %v", cmd.Env)
	}
	if !slices.Contains(cmd.Env, "PROJECT_ONLY=one") {
		t.Errorf("toolNodeCommand env lost the launch surface's other entries: %v", cmd.Env)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, "/shim-first") {
			t.Errorf("the first composition survived the second: %q", entry)
		}
	}
	script := e.toolNodeScriptCommand(context.Background(), "sh", "x.sh")
	if !slices.Contains(script.Env, "PATH=/shim-second:/project/bin") {
		t.Errorf("toolNodeScriptCommand env misses the engine's composed PATH: %v", script.Env)
	}

	// A launch-surface push AFTER the engine composed does not drop the
	// engine's PATH: the engine layer is merged last.
	e.SetRunExtraEnv([]string{"PATH=/late/bin", "LATE=yes"})
	cmd = e.toolNodeCommand(context.Background(), "true", nil)
	if !slices.Contains(cmd.Env, "PATH=/shim-second:/project/bin") {
		t.Errorf("a later launch-surface PATH push dropped the engine's composed PATH: %v", cmd.Env)
	}
	if !slices.Contains(cmd.Env, "LATE=yes") {
		t.Errorf("the later launch-surface push's other entries are missing: %v", cmd.Env)
	}
	if got := e.GetRunExtraEnvValue("PATH"); got != "/late/bin" {
		t.Errorf("launch-surface PATH = %q after the later push, want /late/bin", got)
	}
}
