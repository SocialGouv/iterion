package model

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// canaryEngineBin installs an executable file and points ITERION_BIN at it,
// so proc.LocateIterionBinary resolves deterministically inside a test
// binary: the sibling-of-Executable arm is skipped on a go-build path and
// the override is next. The returned path is the canary's.
func canaryEngineBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "iterion")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", bin)
	return bin
}

// recordingSandboxRun captures the ExecOpts.Env of every Command so a test
// can read the environment the command would receive.
type recordingSandboxRun struct {
	envs []map[string]string
}

func (r *recordingSandboxRun) Driver() string { return "recording" }
func (r *recordingSandboxRun) Command(ctx context.Context, _ []string, opts sandbox.ExecOpts) *exec.Cmd {
	copied := make(map[string]string, len(opts.Env))
	for k, v := range opts.Env {
		copied[k] = v
	}
	r.envs = append(r.envs, copied)
	return exec.CommandContext(ctx, "true")
}
func (r *recordingSandboxRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (r *recordingSandboxRun) Cleanup(context.Context) error { return nil }

// isolatedSandboxRun answers true: its commands execute in a process
// namespace of their own, like the docker and kubernetes drivers.
type isolatedSandboxRun struct{ recordingSandboxRun }

func (*isolatedSandboxRun) ProcessIsolated() bool { return true }

// sharedPidSandboxRun models the docker driver under --pid=host: its
// commands share the operator's process namespace, but they still resolve
// paths inside the container's filesystem.
type sharedPidSandboxRun struct{ recordingSandboxRun }

func (*sharedPidSandboxRun) ProcessIsolated() bool { return false }

// noopLikeSandboxRun models the passthrough: its commands execute on the
// operator's host, so paths resolve against the HOST filesystem.
type noopLikeSandboxRun struct{ recordingSandboxRun }

func (*noopLikeSandboxRun) HostExecutesCommands() bool { return true }

// Both tool-node command builders hand the bot script the engine's own
// binary: the value is the ITERION_BIN resolution, absolute, and never a
// workspace-relative name. A script that re-enters the CLI resolves it
// from here — a workspace fallback would execute the tree under audit
// (#1799).
func TestToolNodeCommandsCarryTheEngineBinaryPath(t *testing.T) {
	bin := canaryEngineBin(t)
	e := &ClawExecutor{}

	cmd := e.toolNodeCommand(context.Background(), "true", nil)
	if got := envValue(cmd.Env, sandbox.EngineBinaryEnvVar); got != bin {
		t.Fatalf("shell command %s = %q, want %q", sandbox.EngineBinaryEnvVar, got, bin)
	}

	sc := e.toolNodeScriptCommand(context.Background(), []string{"python3", "-I"}, "scope_check.py")
	if got := envValue(sc.Env, sandbox.EngineBinaryEnvVar); got != bin {
		t.Fatalf("script command %s = %q, want %q", sandbox.EngineBinaryEnvVar, got, bin)
	}
}

// A command that really executes inside a sandbox gets the container path
// the bind mount and the runner image both place the binary at — never the
// host resolution, which names a file the container cannot see.
func TestSandboxedToolNodeGetsTheContainerBinaryPath(t *testing.T) {
	bin := canaryEngineBin(t)
	e := &ClawExecutor{}
	fake := &isolatedSandboxRun{}
	e.SetSandbox(fake)

	e.toolNodeCommand(context.Background(), "true", nil)
	e.toolNodeScriptCommand(context.Background(), []string{"python3", "-I"}, "scope_check.py")
	if len(fake.envs) != 2 {
		t.Fatalf("recorded %d commands, want 2", len(fake.envs))
	}
	for i, env := range fake.envs {
		if got := env[sandbox.EngineBinaryEnvVar]; got != sandbox.EngineBinaryContainerPath {
			t.Fatalf("sandboxed command %d: %s = %q, want the container path %q (host resolution %q would not resolve in-container)",
				i, sandbox.EngineBinaryEnvVar, got, sandbox.EngineBinaryContainerPath, bin)
		}
	}
}

// A container whose commands share the operator's process namespace still
// resolves paths inside the CONTAINER filesystem: --pid=host changes whose
// signals reach whom, not which /usr/local/bin exists. It gets the
// container path like any other container run.
func TestSharedPidSandboxToolNodeStillGetsTheContainerBinaryPath(t *testing.T) {
	bin := canaryEngineBin(t)
	e := &ClawExecutor{}
	fake := &sharedPidSandboxRun{}
	e.SetSandbox(fake)

	e.toolNodeCommand(context.Background(), "true", nil)
	e.toolNodeScriptCommand(context.Background(), []string{"python3", "-I"}, "scope_check.py")
	if len(fake.envs) != 2 {
		t.Fatalf("recorded %d commands, want 2", len(fake.envs))
	}
	for i, env := range fake.envs {
		if got := env[sandbox.EngineBinaryEnvVar]; got != sandbox.EngineBinaryContainerPath {
			t.Fatalf("shared-pid command %d: %s = %q, want the container path %q (the host resolution %q does not exist in-container)",
				i, sandbox.EngineBinaryEnvVar, got, sandbox.EngineBinaryContainerPath, bin)
		}
	}
}

// The passthrough's commands run on the host, so it gets the host
// resolution — the container path would name a file the host may carry
// under a different build than the engine running the nodes.
func TestNoopSandboxToolNodeGetsTheHostBinaryPath(t *testing.T) {
	bin := canaryEngineBin(t)
	e := &ClawExecutor{}
	fake := &noopLikeSandboxRun{}
	e.SetSandbox(fake)

	e.toolNodeCommand(context.Background(), "true", nil)
	e.toolNodeScriptCommand(context.Background(), []string{"python3", "-I"}, "scope_check.py")
	if len(fake.envs) != 2 {
		t.Fatalf("recorded %d commands, want 2", len(fake.envs))
	}
	for i, env := range fake.envs {
		if got := env[sandbox.EngineBinaryEnvVar]; got != bin {
			t.Fatalf("noop command %d: %s = %q, want the host resolution %q", i, sandbox.EngineBinaryEnvVar, got, bin)
		}
	}
}
