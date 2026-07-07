package delegate

import (
	"context"
	"fmt"
	"strings"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Sandboxed claude_code subprocess lifecycle (native:221edac8).
//
// Under a sandbox the claude CLI runs INSIDE the container while the SDK
// only holds the host-side `docker exec` client. Killing that client on a
// stream abort (cold/idle timeout) does NOT terminate the in-container
// claude — docker exec has no signal path to the exec'd process. Observed
// live on the 2026-07-07 dogfood party: 4-5 leaked `claude --print`
// processes stacked in one container, each still consuming the shared
// forfait and starving the next retry into the same cold timeout.
//
// The fix records the in-container PID at spawn time and kills it through
// a second, short-lived exec when the session ends. The wrapper writes the
// shell's own PID to a pidfile and then `exec`s the real command — exec
// keeps the PID and the stdin/stdout wiring, so the recorded PID IS
// claude's and the stream-json stdin protocol is untouched.

// sandboxDelegateMark derives a per-invocation marker used to name the
// pidfile. Node id + iteration + a nanosecond suffix keeps concurrent
// nodes and rapid retries distinct within one container.
func sandboxDelegateMark(task Task) string {
	node := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, task.NodeID)
	if node == "" {
		node = "node"
	}
	return fmt.Sprintf("%s-%d-%d", node, task.Iteration, time.Now().UnixNano()%1_000_000_000)
}

// sandboxDelegatePIDFile is the in-container pidfile path for a mark.
// /tmp is writable in every sandbox image regardless of the pinned User.
func sandboxDelegatePIDFile(mark string) string {
	return "/tmp/iterion-delegate-" + mark + ".pid"
}

// wrapSandboxDelegateArgv wraps the delegated CLI invocation so its
// in-container PID lands in the pidfile before exec replaces the shell
// with the real command (same PID, same fds — the NDJSON stdin protocol
// is unaffected). POSIX-sh only: no bashisms, dash-safe.
func wrapSandboxDelegateArgv(mark string, argv []string) []string {
	script := "echo $$ > " + sandboxDelegatePIDFile(mark) + " && exec \"$@\""
	return append([]string{"sh", "-c", script, "iterion-delegate"}, argv...)
}

// killSandboxDelegate returns a cleanup that terminates the recorded
// in-container process: TERM, a grace second, then KILL, then removes the
// pidfile. Idempotent and best-effort — after a clean exit the PID is
// gone and every kill is a no-op; failures only log. The cleanup uses its
// own timeout context so it still runs when the session context is
// already cancelled (the abort path is exactly when it matters).
func killSandboxDelegate(run sandbox.Run, mark string, logger *iterlog.Logger) func() {
	pidFile := sandboxDelegatePIDFile(mark)
	script := fmt.Sprintf(
		"[ -f %[1]s ] || exit 0; P=$(cat %[1]s); rm -f %[1]s; "+
			"kill -0 \"$P\" 2>/dev/null || exit 0; "+
			"kill -TERM \"$P\" 2>/dev/null; sleep 1; "+
			"kill -0 \"$P\" 2>/dev/null && kill -KILL \"$P\" 2>/dev/null; true",
		pidFile,
	)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := run.Command(ctx, []string{"sh", "-c", script}, sandbox.ExecOpts{})
		if err := cmd.Run(); err != nil {
			logger.Warn("claude-code: sandbox delegate cleanup (%s) failed: %v", mark, err)
		}
	}
}
