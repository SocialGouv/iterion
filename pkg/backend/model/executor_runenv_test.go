package model

import (
	"context"
	"os/exec"
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

func TestInvocationFilesOverrideHostToolCommandsPerCall(t *testing.T) {
	e := &ClawExecutor{artifactFilesDir: "/legacy"}
	first := WithInvocationFiles(context.Background(), InvocationFiles{HostDir: "/attempt-a", SandboxDir: "/sandbox-a"})
	second := WithInvocationFiles(context.Background(), InvocationFiles{HostDir: "/attempt-b", SandboxDir: "/sandbox-b"})
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{
		{first, "ITERION_ARTIFACT_FILES_DIR=/attempt-a"},
		{second, "ITERION_ARTIFACT_FILES_DIR=/attempt-b"},
		{context.Background(), "ITERION_ARTIFACT_FILES_DIR=/legacy"},
	} {
		for _, cmd := range []*exec.Cmd{
			e.toolNodeCommand(tc.ctx, "true", map[string]string{"ITERION_ARTIFACT_FILES_DIR": "/untrusted"}),
			e.toolNodeScriptCommand(tc.ctx, "sh", "x.sh"),
		} {
			if len(cmd.Env) == 0 {
				t.Fatal("missing tool environment")
			}
			if cmd.Env[len(cmd.Env)-1] != tc.want {
				t.Fatalf("last file environment is %q, want %q", cmd.Env[len(cmd.Env)-1], tc.want)
			}
		}
	}
}

func TestInvocationInputPathsAreScopedWithoutChangingLogicalReferences(t *testing.T) {
	logical := map[string]any{"source": map[string]any{"path": "published/input/0/abc", "sha256": "abc"}, "nested": []any{map[string]any{"path": "other"}}}
	paths := map[string]InvocationFileInput{"published/input/0/abc": {HostPath: "/host/attempt/input", SandboxPath: "/sandbox/attempt/input", SHA256: "abc"}}
	for _, tc := range []struct {
		sandbox bool
		want    string
	}{{false, "/host/attempt/input"}, {true, "/sandbox/attempt/input"}} {
		resolved := invocationInputPaths(logical, paths, tc.sandbox).(map[string]any)
		if got := resolved["source"]; got != tc.want {
			t.Fatalf("materialized path = %v, want %s", got, tc.want)
		}
		if got := resolved["nested"].([]any)[0].(map[string]any)["path"]; got != "other" {
			t.Fatalf("unrelated path changed: %v", got)
		}
	}
	if got := logical["source"].(map[string]any)["path"]; got != "published/input/0/abc" {
		t.Fatalf("logical checkpoint input was mutated: %v", got)
	}
}
