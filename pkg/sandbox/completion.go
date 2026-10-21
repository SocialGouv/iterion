package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
)

// A command the sandbox runs is reported by an exec client — `kubectl exec`,
// `docker exec` — whose exit status is not proof that the command ended. A
// stream lost behind the client (the API server's WebSocket-to-SPDY
// translator and the kubelet behind it, a kubelet or container runtime
// restart) ends the session with status 0 and whatever output had arrived,
// while the command goes on running in the container: client-go reads an
// error stream that closed empty as success. Measured on a 3.5-hour gate
// script: the node finished after 52 minutes with an empty result and no
// error, and its process kept applying mutants to the workspace the next
// node worked in.
//
// Completion makes the command prove its end instead.

// ErrExecStatusUnknown reports a command the sandbox ran whose end was never
// proven: its exec session ended without the completion line the command
// prints on its way out, so whether it finished, and with which status, is
// unknown.
var ErrExecStatusUnknown = errors.New("sandbox: the command's end was never proven: its exec session ended without the command's completion line")

// completionLabel is the $0 of the wrapper shell: the name a process listing
// shows for it.
const completionLabel = "iterion-tool"

// Completion is the proof of its own end that a command run in the sandbox
// carries. Wrap runs the command in a node session of its own (ADR-127: every
// process it starts carries the session's variable) and prints on stdout,
// after everything the command wrote there, one line holding a random nonce
// and the command's exit status. Settle reads that line back out of stdout:
// found, the command ended and the status is its own; missing, the session
// ended before the command did, or its end never arrived. Sweep stops what
// the command's session still runs in the container.
//
// The line goes to stdout, not stderr: an exec multiplexes the two streams,
// and only bytes of one stream arrive in order — a completion line on stdout
// proves that every byte the command wrote to stdout before it arrived too.
type Completion struct {
	mark  string
	nonce string
}

// NewCompletion draws the completion of one command. label names it in the
// container (the session's files, the sweep's report); the random token
// keeps two commands of one container distinct.
func NewCompletion(label string) *Completion {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, label)
	if name == "" {
		name = "tool"
	}
	return &Completion{
		mark:  name + "-" + proc.NewNodeSessionToken(),
		nonce: "iterion-exec-end-" + proc.NewNodeSessionToken(),
	}
}

// SessionEnv is the variable the command's exec must carry in its own
// environment — not exported by the wrapper — so that the wrapper shell is
// of the session too, and a sweep stops it with the command.
func (c *Completion) SessionEnv() (key, value string) {
	return proc.NodeSessionVar(c.mark), "1"
}

// sinceFile dates the session in the container (proc.BootTicks): a sweep
// reports the unreadable processes started since then.
func (c *Completion) sinceFile() string {
	return "/tmp/iterion-tool-" + c.mark + ".since"
}

// Wrap returns argv run under the completion protocol: a POSIX sh program
// that dates the session, runs argv as its child — stdin, stdout and stderr
// inherited — and, once argv has exited, prints the completion line and exits
// with argv's status. The argv stays out of the wrapper's text: it follows as
// the program's positional parameters, so nothing in it is ever parsed as
// shell.
func (c *Completion) Wrap(argv []string) []string {
	since := shellquote.Quote(c.sinceFile())
	script := "[ -f " + since + " ] || { read -r up _ < /proc/uptime && echo \"${up%.*}${up#*.}\" > " + since + "; } 2>/dev/null\n" +
		"\"$@\"\n" +
		"rc=$?\n" +
		"rm -f " + since + "\n" +
		"printf '\\n%s %s\\n' " + shellquote.Quote(c.nonce) + " \"$rc\"\n" +
		"exit \"$rc\""
	return append([]string{"sh", "-c", script, completionLabel}, argv...)
}

// Settle reads the completion line out of stdout. ended reports whether it
// was there; status is then the command's exit status, and clean is stdout
// without the line — byte for byte what the command and its descendants
// wrote, a background child's output after the line included. Without the
// line, clean is stdout as it arrived.
func (c *Completion) Settle(stdout []byte) (clean []byte, status int, ended bool) {
	head := []byte("\n" + c.nonce + " ")
	i := bytes.LastIndex(stdout, head)
	if i < 0 {
		return stdout, 0, false
	}
	rest := stdout[i+len(head):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 1 || nl > 3 {
		return stdout, 0, false
	}
	for _, b := range rest[:nl] {
		if b < '0' || b > '9' {
			return stdout, 0, false
		}
	}
	status, err := strconv.Atoi(string(rest[:nl]))
	if err != nil || status > 255 {
		return stdout, 0, false
	}
	end := i + len(head) + nl + 1
	if end == len(stdout) {
		return stdout[:i], status, true
	}
	clean = make([]byte, 0, len(stdout)-(end-i))
	clean = append(append(clean, stdout[:i]...), stdout[end:]...)
	return clean, status, true
}

// Sweep stops, in the container, every process of the command's session —
// the wrapper, the command, what it started, whatever detached from it — the
// way ADR-127 ends an agent's: one short exec on a context of its own, so it
// still runs when the command's context is gone. It removes the file that
// dated the session. The report names what it stopped; the error, what it
// could not do — an exec that failed, a program that died on its way, a
// survivor of the KILL.
func (c *Completion) Sweep(run Run) (proc.SweepReport, error) {
	since := c.sinceFile()
	sweep, err := proc.NodeSessionSweepScript(c.mark, since)
	if err != nil {
		return proc.SweepReport{}, fmt.Errorf("the processes of session %s cannot be swept: %w", c.mark, err)
	}
	// A subshell: the program's own exit must not skip the rm, and its
	// status — a program killed on its way — is the exec's.
	script := "(\n" + sweep + ")\nrc=$?\nrm -f " + shellquote.Quote(since) + "\nexit $rc"
	// The sweep's TERM and KILL phases are bounded, each running one scan
	// of the container's /proc past its bound, which a crowded container
	// makes long.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, runErr := run.Command(ctx, []string{"sh", "-c", script}, ExecOpts{}).Output()
	report, perr := proc.ParseNodeSessionSweep(string(out))
	if runErr != nil {
		perr = errors.Join(perr, fmt.Errorf("the exec that sweeps session %s failed: %w", c.mark, runErr))
	}
	return report, errors.Join(perr, report.Problems())
}
