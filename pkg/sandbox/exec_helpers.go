package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
//
// When logger is non-nil the snippet's stdout and stderr are also
// streamed to the logger as Info / Warn lines so operators see
// install progress live in run.log instead of waiting for a terminal
// failure to dump a buffered blob. Pass nil to suppress streaming.
func RunPostCreate(ctx context.Context, run Run, snippet string, logger *iterlog.Logger) error {
	opts := ExecOpts{}
	var bufOut, bufErr bytes.Buffer
	if logger != nil {
		// Stream + buffer in parallel so the on-error message keeps the
		// embedded captures it always had.
		opts.Stdout = io.MultiWriter(&bufOut, &lineLogger{l: logger, level: "info"})
		opts.Stderr = io.MultiWriter(&bufErr, &lineLogger{l: logger, level: "warn"})
	}
	res, err := run.Exec(ctx, []string{"sh", "-c", snippet}, opts)
	if err != nil {
		return fmt.Errorf("postCreateCommand: %w", err)
	}
	stdout := res.Stdout
	stderr := res.Stderr
	if logger != nil {
		stdout = bufOut.Bytes()
		stderr = bufErr.Bytes()
	}
	if res.ExitCode != 0 {
		return fmt.Errorf(
			"postCreateCommand exited %d:\nstdout:\n%s\nstderr:\n%s",
			res.ExitCode, string(stdout), string(stderr),
		)
	}
	return nil
}

// lineLogger fans bytes into a leveled iterlog logger one line at a time.
// Designed for live-streaming subprocess output where each newline
// completes a logical message. Partial trailing lines are buffered
// until the next newline arrives or Close() is called (we don't call
// Close because Run.Exec terminates the writer anyway when the process
// exits).
type lineLogger struct {
	l     *iterlog.Logger
	level string
	buf   bytes.Buffer
}

func (lw *lineLogger) Write(p []byte) (int, error) {
	lw.buf.Write(p)
	for {
		idx := bytes.IndexByte(lw.buf.Bytes(), '\n')
		if idx < 0 {
			break
		}
		line := string(lw.buf.Next(idx + 1))
		line = line[:len(line)-1] // drop newline
		if line == "" {
			continue
		}
		switch lw.level {
		case "warn":
			lw.l.Warn("sandbox: postCreate: %s", line)
		default:
			lw.l.Info("sandbox: postCreate: %s", line)
		}
	}
	return len(p), nil
}

// MaxInlineArgBytes caps how large a `sh -c <script>` argument may get
// before a driver routes it through stdin instead of argv. Linux caps a
// SINGLE argv element at MAX_ARG_STRLEN (32 pages = 128 KiB), a limit
// no ulimit raises; exceeding it fails the exec with E2BIG
// ("argument list too long") before the command ever reaches the
// container. 100 KB leaves headroom for the rest of the argv (flags,
// env, pod/container id) and is far above any normal tool snippet.
const MaxInlineArgBytes = 100_000

// ShouldStreamScriptViaStdin reports whether cmd is the
// `sh -c <script>` shape and should be routed through stdin instead of
// argv to avoid E2BIG. Returns the script when so; "" otherwise.
//
// Conditions: cmd is exactly `["sh","-c", script]` or `["bash","-c",
// script]`, no stdin is already attached (so a caller-provided reader
// is never clobbered), and the script exceeds [MaxInlineArgBytes].
// Both shells are matched because internal callers emit both: the
// tool-node executor runs recipes via `bash -c`, while RunPostCreate
// and the claw bash builtin use `sh -c`. Any other shell or argv shape
// falls through to the standard argv path so behaviour is byte-for-byte
// unchanged. Callers re-use cmd[0] for the `-s` invocation, so bash
// recipes keep bash semantics.
//
// Shared by the docker and kubernetes drivers: both fork a host binary
// whose argv carries the script, so both are subject to the same
// kernel limit. A driver that grew its own copy of this predicate
// would drift from the other the first time the threshold moved.
func ShouldStreamScriptViaStdin(cmd []string, opts ExecOpts) string {
	if len(cmd) != 3 || (cmd[0] != "sh" && cmd[0] != "bash") || cmd[1] != "-c" {
		return ""
	}
	if opts.Stdin != nil {
		return ""
	}
	if len(cmd[2]) <= MaxInlineArgBytes {
		return ""
	}
	return cmd[2]
}
