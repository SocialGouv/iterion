package dryrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrNoChecker says an interpreter has no syntax checker here: the text is
// reported unchecked, never guessed at.
var ErrNoChecker = errors.New("dryrun: no syntax checker for this interpreter")

// ErrCheckTimeout says the checker did not answer in time: unchecked, said
// as such — never a syntax verdict.
var ErrCheckTimeout = errors.New("dryrun: the syntax check timed out")

// ShellChecker holds an interpreter's text to its syntax without running
// it. Check returns nil when the text parses, ErrNoChecker (wrapped or not)
// when the interpreter is not one it checks, and the interpreter's own
// message otherwise.
type ShellChecker interface {
	Check(interpreter, text string) error
}

// Bash checks shell text with an interpreter's own `-n` — a parse, not a
// run: the text is fed on stdin and nothing in it executes. A `command:` is
// checked as bash, which the executor pins it to; a `script:` as its
// `language:` — and `sh`, the default, as **dash**: that is the `sh` of the
// images iterion ships, whatever `sh` is on this host (here it may be bash,
// which accepts what dash refuses — the cross-shell trap). Without dash on
// the PATH, sh text is unchecked, said.
type Bash struct {
	// Timeout bounds one check; zero is five seconds.
	Timeout time.Duration
}

// Check implements ShellChecker.
func (b Bash) Check(interpreter, text string) error {
	var name string
	switch strings.ToLower(strings.TrimSpace(interpreter)) {
	case "bash":
		name = "bash"
	case "sh", "":
		if _, err := exec.LookPath("dash"); err != nil {
			return fmt.Errorf("%w: sh text is held to dash, which is not on this PATH (the sh here may be bash, which accepts what dash refuses)", ErrNoChecker)
		}
		name = "dash"
	default:
		return ErrNoChecker
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, "-n")
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %s -n after %s", ErrCheckTimeout, name, timeout)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		// The interpreter itself is missing or could not start: unchecked,
		// with the reason.
		return fmt.Errorf("%w: %s: %v", ErrNoChecker, name, err)
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(msg)
}
