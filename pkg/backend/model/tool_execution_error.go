package model

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
)

// ToolExecutionError keeps a direct tool failure distinct from a provider
// refusal even when stderr contains HTTP statuses or provider error names.
type ToolExecutionError struct{ Cause error }

func (e *ToolExecutionError) Error() string { return e.Cause.Error() }
func (e *ToolExecutionError) Unwrap() error { return e.Cause }

// toolScriptTempPrefix starts the name of the temp file a `script:` body runs
// from; os.CreateTemp appends a random number and the language's extension.
// pkg/treenoise spells the same prefix for the tree-noise pathspecs.
const toolScriptTempPrefix = ".iterion-script-"

// toolScriptTempName matches that file's name up to its extension.
var toolScriptTempName = regexp.MustCompile(regexp.QuoteMeta(toolScriptTempPrefix) + `[0-9]+`)

// ExitStatus is the exit status of the process the failure ended, and whether
// the chain carries one. A registry tool, a refusal before the command
// started and a sandboxed command whose exec client reported no status carry
// none; a process a signal ended reports -1, as exec does.
func (e *ToolExecutionError) ExitStatus() (int, bool) {
	var exit *exec.ExitError
	if errors.As(e.Cause, &exit) && exit != nil {
		return exit.ExitCode(), true
	}
	return 0, false
}

// StableText is the failure's text as two executions of an unchanged failure
// both print it: the one value the executor itself draws afresh for each
// execution — the random name of a `script:` body's temp file, which an
// interpreter quotes in its own errors (a syntax error, a traceback) — is
// replaced by its pattern. Whatever the COMMAND printed stays as it is: output
// that differs between two executions is a different text.
func (e *ToolExecutionError) StableText() string {
	return toolScriptTempName.ReplaceAllString(e.Error(), toolScriptTempPrefix+"*")
}

// CommandFailure returns the command failure the chain carries, and whether
// it does: a tool node whose command ran and failed with its two streams kept
// apart. A registry tool, an `action:` and a refusal before the command
// started carry none — their error is a single text with no stream to split.
func (e *ToolExecutionError) CommandFailure() (*ToolCommandFailure, bool) {
	if e == nil || e.Cause == nil {
		return nil, false
	}
	var cmd *ToolCommandFailure
	if errors.As(e.Cause, &cmd) && cmd != nil {
		return cmd, true
	}
	return nil, false
}

// ToolCommandFailure is a tool node's command that ran and failed: the node
// it belongs to, the executor's own wording of how it ended (Outcome), the
// process's error (Cause, nil when the node's verdict is not the recipe's
// own failure), and the two streams the command printed.
//
// Error() is the exact text the executor has always quoted — node id,
// outcome, cause and both streams. The run document, the identical-failure
// fingerprint and the event log read it, and a reader that diffed two
// executions of an unchanged failure would see a change. A reader that must
// not trust the command's stdout takes Diagnostics() or Output() instead:
// stdout is the command's DATA, and a network wording echoed there — a curl
// verdict, a test's `dial tcp …: connection refused` — says nothing about
// the run's connectivity.
type ToolCommandFailure struct {
	NodeID  string
	Outcome string
	Cause   error
	Stdout  string
	Stderr  string
}

func (e *ToolCommandFailure) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("model: tool node %q: %s\nstdout: %s\nstderr: %s", e.NodeID, e.Outcome, e.Stdout, e.Stderr)
	}
	return fmt.Sprintf("model: tool node %q: %s: %s\nstdout: %s\nstderr: %s", e.NodeID, e.Outcome, e.Cause, e.Stdout, e.Stderr)
}

// Unwrap exposes the process's own error, so a reader of the chain still
// finds the exit status (exec.ExitError) and the sandbox's
// ErrExecStatusUnknown through the failure.
func (e *ToolCommandFailure) Unwrap() error { return e.Cause }

// Diagnostics is what the command reported about WHY it failed: the exec
// error's text and stderr. Never stdout, never the wrapper's node id or
// outcome.
func (e *ToolCommandFailure) Diagnostics() string {
	if e.Cause == nil {
		return e.Stderr
	}
	return e.Cause.Error() + "\n" + e.Stderr
}

// Output is the whole command output, diagnostics first: Diagnostics() plus
// stdout, still without the wrapper's own text.
func (e *ToolCommandFailure) Output() string {
	return e.Diagnostics() + "\n" + e.Stdout
}
