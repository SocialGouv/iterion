// Package noop provides the always-available passthrough sandbox
// driver.
//
// The noop driver advertises empty [sandbox.Capabilities] and runs
// [sandbox.Run.Exec] commands on the host directly via os/exec. It is
// the fallback when:
//
//   - the user does not request a sandbox (the default), or
//   - the requested driver is unavailable on this host (e.g. desktop
//     without docker), or
//   - in cloud V1 mode, where the runner pod is the de-facto sandbox
//     and per-run isolation is deferred to V2.
//
// Calling [sandbox.Driver.Prepare] with a [sandbox.Spec] whose mode is
// active ([sandbox.ModeAuto] or [sandbox.ModeInline]) returns a
// special [PreparedSpec] that records the skip reason. The engine
// emits a `sandbox_skipped` event when it sees this — that's the
// observability hook that makes the noop fallback visible to
// operators.
package noop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// New returns a fresh noop [sandbox.Driver]. It is always
// constructible — never returns an error.
func New() (sandbox.Driver, error) {
	return &driver{}, nil
}

// Constructor is the [sandbox.DriverConstructor] hook for registration
// in [sandbox.Factory].
func Constructor() (sandbox.Driver, error) { return New() }

// driver implements [sandbox.Driver].
type driver struct{}

// Name returns "noop".
func (d *driver) Name() string { return "noop" }

// Capabilities advertises no support for any feature: the engine
// validates against this and emits clear errors when a workflow
// demands a feature the noop driver cannot provide.
func (d *driver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{}
}

// Prepare validates the spec and records the skip reason. Active
// modes ([sandbox.ModeAuto] / [sandbox.ModeInline]) produce a
// PreparedSpec with [Prepared.SkippedReason] populated; inactive
// modes succeed silently.
func (d *driver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	p := &Prepared{spec: spec}
	if spec.Mode.IsActive() {
		p.SkippedReason = fmt.Sprintf("noop driver does not implement mode=%s; falling back to host execution", spec.Mode)
	}
	return p, nil
}

// Start returns a host-execution [sandbox.Run]. It does not allocate
// any resources beyond an in-memory state struct — Cleanup is a nop.
func (d *driver) Start(_ context.Context, prepared sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	p, ok := prepared.(*Prepared)
	if !ok {
		return nil, fmt.Errorf("noop: PreparedSpec from driver %q passed to noop.Start", prepared.DriverName())
	}
	return &run{prepared: p, info: info}, nil
}

// Prepared is the noop driver's [sandbox.PreparedSpec] implementation.
//
// SkippedReason is populated when the user-requested spec asks for an
// active sandbox mode; the engine logs it via the `sandbox_skipped`
// event so the absence of isolation is auditable.
type Prepared struct {
	spec          sandbox.Spec
	SkippedReason string
}

// DriverName implements [sandbox.PreparedSpec].
func (p *Prepared) DriverName() string { return "noop" }

// Spec returns the resolved spec the driver was prepared with. Useful
// for tests and for the engine to emit the skipped event with the
// requested mode.
func (p *Prepared) Spec() sandbox.Spec { return p.spec }

// run implements [sandbox.Run] for the noop driver — host execution.
type run struct {
	prepared *Prepared
	info     sandbox.RunInfo
}

// Driver returns "noop".
func (r *run) Driver() string { return "noop" }

// Command returns a host *exec.Cmd. The noop driver is a transparent
// passthrough — the returned cmd is what callers would build with
// exec.CommandContext, with WorkDir and Env folded in.
func (r *run) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	if len(cmd) == 0 {
		// Match exec.CommandContext's "Path is empty -> Err set on
		// Start" behaviour by returning an exec.Cmd with no path. We
		// cannot return nil because callers expect a usable cmd.
		return exec.CommandContext(ctx, "")
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	if opts.WorkDir != "" {
		c.Dir = opts.WorkDir
	} else if r.info.WorkspacePath != "" {
		c.Dir = r.info.WorkspacePath
	}
	c.Env = os.Environ()
	for k, v := range opts.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	if opts.Stdin != nil {
		c.Stdin = opts.Stdin
	}
	return c
}

// Exec runs the command on the host via os/exec. Stdin/Stdout/Stderr
// are wired through unchanged when provided; otherwise output is
// captured into the returned [sandbox.ExecResult].
func (r *run) Exec(ctx context.Context, cmd []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if len(cmd) == 0 {
		return sandbox.ExecResult{}, fmt.Errorf("noop.Exec: empty cmd")
	}

	c := r.Command(ctx, cmd, opts)

	var stdoutBuf, stderrBuf bytes.Buffer
	if opts.Stdout != nil {
		c.Stdout = opts.Stdout
	} else {
		c.Stdout = &stdoutBuf
	}
	if opts.Stderr != nil {
		c.Stderr = opts.Stderr
	} else {
		c.Stderr = &stderrBuf
	}

	err := c.Run()
	res := sandbox.ExecResult{
		ExitCode: c.ProcessState.ExitCode(),
	}
	if opts.Stdout == nil {
		res.Stdout = stdoutBuf.Bytes()
	}
	if opts.Stderr == nil {
		res.Stderr = stderrBuf.Bytes()
	}

	// exec.ExitError is reported as a non-zero exit code — not a
	// driver-level error. Only context cancellation, missing binary,
	// etc. surface as the returned err.
	if _, isExit := err.(*exec.ExitError); isExit {
		err = nil
	}
	return res, err
}

// Stop is a nop for the noop driver.
func (r *run) Stop(_ context.Context) error { return nil }

// Cleanup is a nop for the noop driver.
func (r *run) Cleanup(_ context.Context) error { return nil }

// Compile-time interface checks.
var (
	_ sandbox.Driver       = (*driver)(nil)
	_ sandbox.Run          = (*run)(nil)
	_ sandbox.PreparedSpec = (*Prepared)(nil)
)
