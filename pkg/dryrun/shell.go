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

// ShellChecker holds an interpreter's text to its syntax without running
// it. Check returns nil when the text parses, ErrNoChecker (wrapped or not)
// when the interpreter is not one it checks, and the interpreter's own
// message otherwise.
type ShellChecker interface {
	Check(interpreter, text string) error
}

// Bash checks `bash` and `sh` text with the interpreter's own `-n` — a
// parse, not a run: the text is fed on stdin and nothing in it executes.
// The executor pins tool commands to `bash -c`, so a command is checked
// as bash; a script is checked as its `language:`.
type Bash struct {
	// Timeout bounds one check; zero is five seconds.
	Timeout time.Duration
}

// Check implements ShellChecker.
func (b Bash) Check(interpreter, text string) error {
	var name string
	switch strings.ToLower(strings.TrimSpace(interpreter)) {
	case "bash", "":
		name = "bash"
	case "sh":
		name = "sh"
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
