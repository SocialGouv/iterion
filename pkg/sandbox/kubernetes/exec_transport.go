package kubernetes

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/execenv"
	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

// execEnvCommand retains the caller's streams and transports overrides as data.
// The capsule will capture the environment of the actual pod invocation; Spec
// Env, image metadata and workload values never choose deployment authority.
func (r *Run) execEnvCommand(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	if r.driver.transportErr != nil {
		return &exec.Cmd{Err: r.driver.transportErr}
	}
	cfg := r.driver.transport
	args := slices.Clone(argv)
	stdin := opts.Stdin
	if script := sandbox.ShouldStreamScriptViaStdin(argv, opts); script != "" {
		args = []string{argv[0], "-s"}
		stdin = strings.NewReader(script)
	}
	in := protocol.Invocation{Argv: args, Env: maps.Clone(opts.Env), Profile: &cfg.profile}
	if opts.WorkDir != "" && opts.WorkDir != r.prepared.workspace {
		dir := opts.WorkDir
		in.WorkDir = &dir
	}
	// Keep the existing per-command operator override. Its documented warning
	// policy belongs to this boundary, not to the pure NewCommand constructor.
	var drain exec.Cmd
	waitDelay := proc.SetWaitDelay(&drain, kubectlDrainBound)
	cmd := execenv.NewCommand(ctx, execenv.Launch{
		HostHelper:   cfg.hostHelper,
		ClientPath:   string(r.driver.kubectl),
		ClientArgs:   []string{"--namespace", r.namespace, "exec", "--stdin", r.podName, "--container", "workload", "--", cfg.guestHelper},
		ControlEnv:   os.Environ(),
		Invocation:   in,
		StartTimeout: cfg.startTimeout,
		WaitDelay:    waitDelay,
	}, stdin)
	if cmd.Err == nil && cfg.startTimeout == 0 && r.driver.logger != nil {
		r.driver.logger.Warn("kubernetes: exec startup is unbounded; set ITERION_SANDBOX_K8S_EXEC_START_TIMEOUT to bound guest readiness")
	}
	return cmd
}
