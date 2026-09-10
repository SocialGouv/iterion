package cli

import (
	"errors"
	"fmt"
)

// ErrUserInput wraps an underlying error to mark it as caused by a
// bad user-supplied value (flag, argument, file path, env var) rather
// than an internal failure. The CLI entry point uses this to map the
// error to exit code 2 ("usage error") instead of the generic exit 1
// ("internal error"), matching the convention shared by most modern
// CLIs.
//
// Wrap at the call site where the user-facing validation rejected the
// input:
//
//	if err := git.ValidateBranchName(opts.BranchName); err != nil {
//	    return cli.UserInputError(fmt.Errorf("--branch-name: %w", err))
//	}
//
// errors.Is(err, ErrUserInput) reports whether any wrapper in the
// chain marked it as a user-input failure.
var ErrUserInput = errors.New("user input")

// ErrReported marks a failure whose diagnosis the command has ALREADY
// written to its output — a `--json` result carrying `valid: false` and
// its diagnostics. The CLI exits non-zero on it without printing a second
// JSON document after the first, which would break every parser reading
// the stream.
var ErrReported = errors.New("already reported")

// UserInputError wraps err with ErrUserInput so the CLI exits with
// status 2. Returns nil when err is nil so the caller can chain
// `return cli.UserInputError(maybeErr)` without a nil check.
func UserInputError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrUserInput, err)
}
