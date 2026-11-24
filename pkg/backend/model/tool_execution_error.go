package model

import (
	"errors"
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
