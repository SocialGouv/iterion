package proctest

import (
	"bytes"
	"fmt"
	"os/exec"
)

// SplitOutput runs cmd and returns its STDOUT alone, with stderr collected
// beside it rather than spliced into it.
//
// A test that PARSES a subprocess's answer — a JSON stream, one record per
// line, a token count — must read the stream the subprocess promised,
// because the other one carries whatever the ENVIRONMENT decided to say: a
// toolchain's notice about a read-only module cache, git's advice and hints,
// a deprecation warning from an interpreter. [exec.Cmd.CombinedOutput]
// interleaves the two, so such a parse renders a verdict on the machine
// instead of on the answer — and the same test is green on a CI runner whose
// tools happen to be quiet. Callers quote the returned stderr in their
// failure message: when the parse has nothing to read, a real diagnostic
// from the subprocess is the reason.
//
// Where the output is only a diagnostic — a failure message, a log line —
// CombinedOutput remains the right instrument: there the whole stream IS
// the message, and nothing parses it.
//
// SplitOutput owns BOTH streams of the command, so it refuses a cmd whose
// Stdout or Stderr is already set rather than running it: silently replacing
// a caller's Stderr would discard the destination it chose, and
// [exec.Cmd.Output] answers a set Stdout with "exec: Stdout already set" —
// an error the caller then reports as the subprocess failing, which is a
// programming mistake dressed as a toolchain failure. The refusal names the
// misuse and the subprocess does not run.
func SplitOutput(cmd *exec.Cmd) (stdout []byte, stderr string, err error) {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, "", fmt.Errorf(
			"proctest.SplitOutput owns both streams of %s and was handed a command that already has one (cmd.Stdout set: %t, cmd.Stderr set: %t): leave both unset, the answer comes back as the return values",
			cmd.Path, cmd.Stdout != nil, cmd.Stderr != nil)
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, runErr := cmd.Output()
	return out, errOut.String(), runErr
}
