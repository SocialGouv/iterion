package delegate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
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

// readSandboxDelegatePID is the shell that sets P to the pid pidFile records,
// or to nothing: the pidfile sits in the container's /tmp, where any process
// of the agent's uid can rewrite it, so only a pid above 1 is trusted — never
// a negative one, which kill reads as a process group, -1 as every process of
// the uid; never 1, the container's init; never a line that would read as a
// sweep's.
func readSandboxDelegatePID(pidFile string) string {
	return "P=; [ -f " + pidFile + " ] && P=$(cat " + pidFile + " 2>/dev/null); " +
		"case $P in ''|*[!0-9]*) P=;; esac; [ -n \"$P\" ] && [ \"$P\" -gt 1 ] || P=\n"
}

// sandboxDelegateCLIPid reads the recorded CLI's pid the exec that ends a
// spawn printed, if it printed one.
func sandboxDelegateCLIPid(out string) (int, bool) {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, sandboxDelegateCLILine); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(rest))
			return pid, err == nil
		}
	}
	return 0, false
}

// sandboxDelegateMark derives the per-invocation mark of a sandboxed spawn. It
// names the pidfile, and it is the node session token every process of the
// spawn inherits (docs/adr/127-an-agent-nodes-processes-end-with-the-node.md).
// Node id + iteration keep it readable; the random suffix keeps concurrent
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
	return fmt.Sprintf("%s-%d-%s", node, task.Iteration, proc.NewNodeSessionToken())
}

// sandboxDelegatePIDFile is the in-container pidfile path for a mark.
// /tmp is writable in every sandbox image regardless of the pinned User.
func sandboxDelegatePIDFile(mark string) string {
	return "/tmp/iterion-delegate-" + mark + ".pid"
}

// sandboxDelegateSinceFile holds, in the container, when the first attempt of
// a mark's spawn began (proc.BootTicks): the sweep reports the unreadable
// processes started since then.
func sandboxDelegateSinceFile(mark string) string {
	return "/tmp/iterion-delegate-" + mark + ".since"
}

// wrapSandboxDelegateArgv wraps the delegated CLI invocation so its
// in-container PID lands in the pidfile before exec replaces the shell
// with the real command (same PID, same fds — the NDJSON stdin protocol
// is unaffected). POSIX-sh only: no bashisms, dash-safe.
//
// The wrapper is also SELF-CLEANING on respawn: the in-executor retry
// loop re-runs the same builder (same mark), so before recording its own
// PID it kills whatever previous attempt the pidfile still points to —
// without this, retry N leaves attempt N-1 running and the leak stacks
// inside a single Execute (observed live: 3 claudes after 2 retries).
//
// It exports the mark's node session variable before the exec, so the CLI and
// everything it starts carry it, whatever they detach from, and dates the
// session once — /proc/uptime holds seconds with two decimals, which make the
// kernel's clock ticks.
func wrapSandboxDelegateArgv(mark string, argv []string) []string {
	pidFile, sinceFile := sandboxDelegatePIDFile(mark), sandboxDelegateSinceFile(mark)
	script := readSandboxDelegatePID(pidFile) + "[ -n \"$P\" ] && kill -KILL \"$P\" 2>/dev/null; " +
		"[ -f " + sinceFile + " ] || { read -r up _ < /proc/uptime && echo \"${up%.*}${up#*.}\" > " + sinceFile + "; } 2>/dev/null; " +
		"echo $$ > " + pidFile + " && export " + proc.NodeSessionVar(mark) + "=1 && exec \"$@\""
	return append([]string{"sh", "-c", script, "iterion-delegate"}, argv...)
}

// killSandboxDelegate returns a cleanup that ends a sandboxed spawn; see
// killSandboxDelegateReporting. The caller that has nowhere to record what
// was stopped still has it said in the run log.
func killSandboxDelegate(run sandbox.Run, mark string, o sweepOwner, spawnFiles ...string) func() {
	end := killSandboxDelegateReporting(run, mark, o, spawnFiles...)
	return func() { end() }
}

// sandboxDelegateCLILine heads the line the exec that ends a spawn prints with
// the recorded CLI's pid: the sweep stops the CLI with the rest of the
// session, and the CLI is the spawn, not something it left.
const sandboxDelegateCLILine = "iterion-delegate-cli "

// killSandboxDelegateReporting returns a cleanup that ends a sandboxed spawn,
// in one short-lived exec: it removes the in-container files the spawn wrote,
// when the caller names any (claude_code's flag settings file); stops the
// mark's node session (proc.NodeSessionSweepScript) — the recorded CLI
// included, so that what the CLI started is found as its descendant before
// the CLI's end orphans it; then terminates the recorded process if it still
// runs — TERM, a grace second, then KILL, for a sweep that could not run —
// and removes the pidfile and the file that dated the session. It returns the
// labels of what the sweep stopped besides the CLI, and says in the run log
// what it did. Idempotent and best-effort — after a clean exit the PID is
// gone and every kill is a no-op. The cleanup uses its own timeout context so
// it still runs when the session context is already cancelled (the abort path
// is exactly when it matters).
func killSandboxDelegateReporting(run sandbox.Run, mark string, o sweepOwner, spawnFiles ...string) func() []string {
	pidFile := sandboxDelegatePIDFile(mark)
	script := ""
	if len(spawnFiles) > 0 {
		quoted := make([]string, len(spawnFiles))
		for i, file := range spawnFiles {
			quoted[i] = shellquote.Quote(file)
		}
		script = "rm -f " + strings.Join(quoted, " ") + "\n"
	}
	script += readSandboxDelegatePID(pidFile) + "[ -n \"$P\" ] && echo " + shellquote.Quote(sandboxDelegateCLILine) + "\"$P\"\n"
	sinceFile := sandboxDelegateSinceFile(mark)
	sweep, sweepErr := proc.NodeSessionSweepScript(mark, sinceFile)
	if sweepErr == nil {
		// A subshell: the program's own exit must not skip what follows, and
		// its status — a program killed on its way — is the exec's.
		script += "(\n" + sweep + ")\nrc=$?\n"
	} else {
		script += "rc=0\n"
	}
	// A subshell again: its early exits must not skip the rm.
	script += "( [ -n \"$P\" ] || exit 0; kill -0 \"$P\" 2>/dev/null || exit 0; " +
		"kill -TERM \"$P\" 2>/dev/null; sleep 1; " +
		"kill -0 \"$P\" 2>/dev/null && kill -KILL \"$P\" 2>/dev/null; true )\n"
	script += "rm -f " + pidFile + " " + shellquote.Quote(sinceFile) + "\nexit $rc"
	return func() []string {
		// The grace second of the kill, then the sweep's TERM and KILL phases,
		// each bounded but each running one scan of the container's /proc past
		// its bound, which a crowded container makes long.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := run.Command(ctx, []string{"sh", "-c", script}, sandbox.ExecOpts{})
		out, err := cmd.Output()
		if sweepErr != nil {
			return reportNodeSessionSweep(o, proc.SweepReport{}, fmt.Errorf("the processes this spawn left cannot be swept: %w", sweepErr))
		}
		r, perr := proc.ParseNodeSessionSweep(string(out))
		// The exec's status is the sweep program's, which exits 0 once it has
		// reported: anything else is a sweep that did not finish, whatever
		// its output says.
		if err != nil {
			perr = errors.Join(perr, fmt.Errorf("the exec that ends the spawn (%s) failed: %w", mark, err))
		}
		if perr != nil {
			perr = fmt.Errorf("the processes this spawn left could not all be found: %w", perr)
		}
		if cli, ok := sandboxDelegateCLIPid(string(out)); ok {
			r.Stopped = slices.DeleteFunc(r.Stopped, func(p proc.SessionProcess) bool { return p.PID == cli })
		}
		return reportNodeSessionSweep(o, r, perr)
	}
}
