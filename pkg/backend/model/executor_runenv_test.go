package model

import (
	"context"
	"slices"
	"testing"
)

// The engine's host devbox provisioning (pkg/runtime/devbox_host.go)
// reaches the executor through a structural interface it type-asserts at
// run start — and SKIPS provisioning entirely when the assert fails.
// This compile-time lock keeps ClawExecutor implementing the seam: if
// the method is renamed or dropped, cloud runs would silently lose their
// bot's devbox toolchain again.
var _ interface{ SetRunExtraEnv(env []string) } = (*ClawExecutor)(nil)

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
