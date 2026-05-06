package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// ExecCmd runs a prepared [*exec.Cmd], wiring stdout/stderr per opts and
// returning a normalised [ExecResult].
//
// Driver implementations of [Run.Exec] typically construct the [*exec.Cmd]
// via their own [Run.Command] (which knows how to wrap docker/kubectl)
// and then defer to this helper to share the buffer-vs-stream plumbing,
// exit-code extraction, and the convention that a non-zero exit code
// surfaces via [ExecResult.ExitCode] (not via a returned error).
//
// Empty stdout/stderr in opts → bytes are buffered into the result.
// Non-nil opts.Stdout / opts.Stderr → bytes are streamed to the writer
// and the result's Stdout/Stderr remain nil.
//
// Errors other than [*exec.ExitError] are returned verbatim. ExitError
// is normalised to (result with ExitCode set, nil error) so callers
// can branch on res.ExitCode without a type assertion.
func ExecCmd(c *exec.Cmd, opts ExecOpts) (ExecResult, error) {
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
	res := ExecResult{}
	if c.ProcessState != nil {
		res.ExitCode = c.ProcessState.ExitCode()
	}
	if opts.Stdout == nil {
		res.Stdout = stdoutBuf.Bytes()
	}
	if opts.Stderr == nil {
		res.Stderr = stderrBuf.Bytes()
	}
	if _, isExit := err.(*exec.ExitError); isExit {
		err = nil
	}
	return res, err
}

// RunPostCreate executes a `sh -c <snippet>` inside the given [Run] and
// returns a friendly error on non-zero exit (with stdout/stderr
// embedded for diagnostics). Used by drivers that honour
// [Spec.PostCreate] — currently docker and kubernetes.
func RunPostCreate(ctx context.Context, run Run, snippet string) error {
	res, err := run.Exec(ctx, []string{"sh", "-c", snippet}, ExecOpts{})
	if err != nil {
		return fmt.Errorf("postCreateCommand: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf(
			"postCreateCommand exited %d:\nstdout:\n%s\nstderr:\n%s",
			res.ExitCode, string(res.Stdout), string(res.Stderr),
		)
	}
	return nil
}
