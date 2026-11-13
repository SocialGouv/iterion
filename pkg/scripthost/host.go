package scripthost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// MaxStderrBytes caps what the parent keeps of a dead child's stderr. A Go
// fatal error prints every goroutine's stack, which is tens of kilobytes of
// noise around one useful line; the head is where that line is.
const MaxStderrBytes = 4 << 10

// stdoutSlack is how much more than the result bound the parent will buffer
// from the child's stdout.
//
// The cap is the point: a child that floods stdout costs the PARENT memory,
// and "the child refuses its own oversized result" is the child's
// self-restraint — exactly what a package about confinement cannot rely on.
// Measured before this bound existed: 256 MiB of child stdout grew the parent
// by 1 GiB of total allocation and took its RSS from 27 MiB to 1033 MiB.
const stdoutSlack = 64 << 10

// Host spawns script-host children. One Host serves many scripts; it holds no
// per-script state, so it is safe for concurrent use (measured: 24 concurrent
// Run calls on one Host, no race, no fd or goroutine growth over 60 sequential
// calls).
type Host struct {
	// Binary is the iterion executable to re-exec. Empty means
	// [proc.LocateIterionBinary], which is the production path.
	Binary string

	// AllowWithoutLandlock lets scripts run on a kernel that offers no
	// Landlock — with an unrestricted filesystem and unrestricted sockets.
	//
	// It travels in the REQUEST and not in the environment, because the
	// child's environment is deliberately empty and an env-var opt-in could
	// never reach it. False by default: the honest posture is that a kernel
	// without Landlock cannot run a script, and this is the greppable escape
	// hatch philosophy 1 asks for.
	AllowWithoutLandlock bool

	// AllowSingleThreadConfinement lets scripts run in a child that could only
	// confine one of its OS threads — see [Request]. False by default, and
	// iterion's own builds are CGO_ENABLED=0, where it never arises.
	AllowSingleThreadConfinement bool

	// Tools, when set, is what a script's calls reach: the node's own tool
	// dispatcher, behind the [ToolServer] seam.
	//
	// Nil is the honest default and not a degraded mode — it is what CM-1a
	// shipped. A child whose parent has no tool server installs no binding at
	// all, so a script that calls one gets "call is not defined" rather than a
	// capability that exists and refuses.
	//
	// One Host still serves many scripts concurrently with this set: the
	// channel and its pump are per-Run, and the server is asked one call at a
	// time per script.
	Tools ToolServer

	// OnClamp, when set, is called with the requested and the actually applied
	// bounds whenever [Limits.Clamp] reduced something.
	//
	// It exists because philosophy 1 forbids silently replacing an operator's
	// explicit choice, and a clamp IS a replacement. The codemode tool wires
	// it to a run-time warning on the node's logger; a nil hook means the
	// reduction is unobserved, which is a gap stated rather than a promise
	// pretended. Nothing can PRODUCE a clamp yet, because no DSL surface
	// lets a `.bot` ask for bounds: the hook is wired ahead of the slice that
	// opens one, and until then its warning has never been printed.
	OnClamp func(requested, applied Limits)
}

// Run executes src in a confined child and returns its verdict.
//
// A non-nil error means the HOST or the CALLER failed: the binary could not be
// started, the deployment answered nothing this code understands, or the
// caller's own context ended. Everything the script or its bounds did comes
// back as a [Response] with a refusal code, because a bound biting is a normal
// outcome a caller branches on, not an exception. A caller that retried a
// refusal would retry a broken deployment forever, which is why the two are
// kept apart.
func (h Host) Run(ctx context.Context, src string, want Limits) (Response, error) {
	limits, clamped := want.Clamp()
	if clamped && h.OnClamp != nil {
		h.OnClamp(want, limits)
	}

	// Refused before the spawn: an oversized script should not cost a process,
	// and the child would have to buffer the whole thing to measure it.
	if int64(len(src)) > limits.ScriptBytes {
		return Refuse(RefusalScriptBytes,
			"the script is %d bytes, over the %d-byte bound", len(src), limits.ScriptBytes), nil
	}

	// The PARENT draws the seed, so the parent holds it: that is the whole
	// content of "a run is replayable". A child drawing its own would make the
	// word mean nothing.
	var rawSeed [32]byte
	if _, err := rand.Read(rawSeed[:]); err != nil {
		return Response{}, fmt.Errorf("scripthost: drawing the script's seed: %w", err)
	}

	payload, err := json.Marshal(Request{
		Script:               src,
		Limits:               limits,
		Seed:                 hex.EncodeToString(rawSeed[:]),
		AllowWithoutLandlock: h.AllowWithoutLandlock,

		AllowSingleThreadConfinement: h.AllowSingleThreadConfinement,

		ToolChannel: h.Tools != nil,
		Proto:       Proto,
	})
	if err != nil {
		return Response{}, fmt.Errorf("scripthost: encoding the request: %w", err)
	}

	binary := h.Binary
	if binary == "" {
		binary = proc.LocateIterionBinary()
	}

	// The CALLER's context is kept: without it, the caller's own (shorter)
	// deadline is indistinguishable from the script's wall bound, and the
	// refusal names a number nobody asked for. Measured before this split: a
	// 400 ms caller deadline on a script with a 5-minute wall bound reported
	// "killed after 5m5s" and advised bounding CPU time instead.
	caller := ctx

	// The parent's deadline is the outer bound on the script. It is a little
	// longer than the child's own wall bound so the child's interrupt gets
	// first refusal: a preemptible script then yields a typed refusal with its
	// own message, and this deadline fires only for the class the interpreter
	// cannot stop.
	ctx, cancel := context.WithTimeout(ctx, limits.Wall+wallGrace)
	defer cancel()

	var stdout, stderr bytes.Buffer
	outCap := &boundedWriter{to: &stdout, limit: int(limits.ResultBytes) + stdoutSlack}
	cmd := command(ctx, binary, payload, outCap, &boundedWriter{to: &stderr, limit: MaxStderrBytes})

	// The tool channel is opened BEFORE the spawn and its parent-side ends are
	// closed AFTER it: a copy left open in the parent would keep the child's
	// read end alive past the child's death, and the pump would then block on
	// a channel nobody will ever write to again.
	var channel *toolChannel
	if h.Tools != nil {
		channel, err = newToolChannel()
		if err != nil {
			return Response{}, err
		}
		defer channel.close()
		channel.attach(cmd)
	}

	if err := cmd.Start(); err != nil {
		return Response{}, fmt.Errorf("scripthost: starting %s %s: %w", binary, Subcommand, err)
	}

	join := func() serveVerdict { return serveVerdict{} }
	if channel != nil {
		channel.closeChildEnds()
		join = pump(ctx, channel, h.Tools, limits)
	}

	waitErr := cmd.Wait()
	serve := join()

	// Inside the teardown window the primitive's own documentation requires:
	// os/exec dispatches the armed Cancel only when the CONTEXT fires, so on
	// the ordinary path — the child exits by itself — nothing ever signals the
	// group. A descendant then outlives Run holding the inherited pipes, and
	// its RLIMIT_CPU accounting restarted at zero when it was forked, so the
	// bound "no parent can lose" does not reach it either.
	if _, err := proc.KillProcessGroup(cmd); err != nil {
		// Not fatal and not silent: the verdict below is still the script's,
		// but a group that refused the signal is worth the stderr line.
		fmt.Fprintf(&stderr, "\nscripthost: killing the child's process group: %v", err)
	}

	// The server aborting outranks every other reading, including a document.
	// It is the one outcome that is neither the script's fault nor the host's:
	// a tool whose verdict needs the operator cannot be answered inside a
	// script, so the thing that must happen next — pause the run, ask — is the
	// CALLER's, and it gets the server's own error rather than a refusal it
	// would have to parse.
	if errors.Is(serve.err, errScriptAborted) {
		return Response{}, serve.err
	}

	// A response that arrived is the verdict, whatever the exit status said:
	// the child writes its document and then exits, so a document means the
	// script ran to a conclusion.
	if resp, ok := decodeResponse(stdout.Bytes()); ok {
		return resp, nil
	}

	// No document, and the channel had already failed: the channel error is
	// what happened, and it is a HOST error — the two sides disagreed about
	// the wire, or the pipe died under a call. Reported ahead of the death
	// evidence because it is the more specific fact, and because the evidence
	// below would otherwise read a channel failure as an unexplained crash.
	if serve.err != nil {
		return Response{}, serve.err
	}

	signalled, signal := deathSignal(cmd)
	return deathToRefusal(deathEvidence{
		callerErr:  caller.Err(),
		scriptErr:  ctx.Err(),
		limits:     limits,
		signalled:  signalled,
		signal:     signal,
		burned:     burnedCPU(cmd),
		waitErr:    waitErr,
		stderrText: stderr.String(),
		stdoutCut:  outCap.cut,
	})
}

// wallGrace is how much longer than the script's own wall bound the parent
// waits. It covers process start, the request read and the response write —
// not script time.
const wallGrace = 5 * time.Second

// deathEvidence is everything the parent observed about a child that produced
// no response.
//
// It carries FACTS rather than the *exec.Cmd they come from, which is what
// makes [deathToRefusal] a pure function of it and testable without a process.
// It held the command once, and the comment claimed the same testability while
// the function reached into it — so the branch ORDER, which that function's own
// comment calls "the whole correctness", had no witness at all: swapping the
// memory and CPU branches left the entire suite green while a probe showed the
// documented catastrophe (a memory death reported as `cpu_time_exceeded` with
// the parent holding `runtime: out of memory`).
type deathEvidence struct {
	callerErr  error // the CALLER's context error, if its own bound ended first
	scriptErr  error // the parent's derived deadline, the script's wall bound
	limits     Limits
	signalled  bool
	signal     syscall.Signal
	burned     time.Duration
	waitErr    error
	stderrText string
	stdoutCut  bool
}

// deathToRefusal names the bound that killed a child that produced no
// response — and names nothing where the evidence names nothing.
//
// The order is the whole correctness of this function, and it was wrong once:
// a threshold on burned CPU time ran BEFORE the positive identification of a
// memory death, so an allocation failure that happened to have burned its CPU
// budget was reported as `cpu_time_exceeded` while the parent held the stderr
// saying `fatal error: runtime: out of memory`. An operator reading that
// raises the CPU bound forever. Positive identification therefore beats a
// threshold, and being SIGNALLED is a necessary condition for a CPU verdict —
// the kernel kills on that bound, so a child that merely exited did not hit
// it.
func deathToRefusal(e deathEvidence) (Response, error) {
	evidence := clip(strings.TrimSpace(firstLines(e.stderrText, 3)), MaxStderrBytes)

	// (0) The CALLER's context, first: its cancellation is neither a script
	// refusal nor a broken deployment, and the parent knows which one fired.
	if e.callerErr != nil {
		return Response{}, fmt.Errorf("scripthost: the caller's context ended before the script did: %w", e.callerErr)
	}

	// (1) The script's own wall bound — the parent's derived deadline, so the
	// parent knows it first-hand. The message names the bound the operator
	// set, not the internal grace it was given.
	if errors.Is(e.scriptErr, context.DeadlineExceeded) {
		return Refuse(RefusalWallClock,
			"the script outlived its %s wall bound and neither it nor the interpreter yielded. "+
				"A native regex or a tight native call cannot be interrupted — bound it with CPU time instead. %s",
			e.limits.Wall, evidence), nil
	}

	// (2) A memory death announces itself: the Go runtime prints before dying.
	// Positive identification, so it outranks every threshold below.
	if looksLikeMemoryDeath(e.stderrText) {
		return Refuse(RefusalMemory,
			"the script host died allocating past its %d-byte memory bound. %s", e.limits.Memory, evidence), nil
	}

	// (3) The CPU bound: the kernel kills on it, so the child must have been
	// signalled, and it must have burned at least what it was allowed. The
	// bound named is the one the kernel enforces — whole seconds — not the
	// sub-second value a caller may have asked for.
	if e.signalled && e.limits.CPU > 0 {
		asked := time.Duration(e.limits.CPUSeconds()) * time.Second
		if e.burned >= asked {
			// Both numbers, because they differ on purpose: the soft limit sits
			// at what the operator asked for and the kernel's SIGKILL one
			// second above it, which is what makes the death attributable.
			// Naming only one left a reader unable to reconcile a 2.002s
			// reading with a "1s" bound.
			return Refuse(RefusalCPUTime,
				"the script burned %s of CPU, over the %s bound the operator set; the kernel kills at %s",
				e.burned.Round(time.Millisecond), e.limits.CPU, asked+time.Second), nil
		}
	}

	// (4) The parent's own stdout cap: the child wrote more than this host
	// will buffer, so whatever document it produced is truncated by US.
	if e.stdoutCut {
		return Refuse(RefusalResultBytes,
			"the script host wrote more than the %d bytes this parent will buffer, so its answer was cut. %s",
			e.limits.ResultBytes+stdoutSlack, evidence), nil
	}

	// (5) Signalled, with nothing naming a bound. Genuinely undecidable from a
	// wait status — a cgroup OOM kill and an operator's `kill` are the same
	// signal — so it is reported as what it is rather than guessed at. The
	// CPU it burned is part of the report: a reader comparing it to the bound
	// is how the ambiguity gets resolved by a human.
	if e.signalled {
		return Refuse(RefusalHostDied,
			"the script host was killed by %v after burning %s of CPU against a %s bound, and nothing "+
				"names a bound; a cgroup memory kill and an external signal are indistinguishable here. %s",
			e.signal, e.burned.Round(time.Millisecond),
			time.Duration(e.limits.CPUSeconds())*time.Second, evidence), nil
	}

	// (6) The child crashed inside the Go runtime. That is the script host
	// failing, not the deployment — so it is a refusal, and the ambiguity is
	// NAMED rather than resolved. Measured: the same allocation bomb prints
	// `runtime: out of memory` under a 128 MiB bound and dies with
	// `SIGSEGV: segmentation violation` under 64 MiB, so a crash under a tight
	// bound is usually the bound and the runtime does not say so. Calling it
	// memory_exceeded would be a guess wearing a verdict.
	if looksLikeRuntimeCrash(e.stderrText) {
		return Refuse(RefusalHostDied,
			"the script host crashed against a %d-byte memory bound and a %s CPU bound, and the "+
				"runtime did not name a cause; a crash under a tight memory bound is usually the bound. %s",
			e.limits.Memory, time.Duration(e.limits.CPUSeconds())*time.Second, evidence), nil
	}

	// (7) Not signalled, no document, no crash, no bound: the deployment
	// answered nothing this code understands — a binary that is not iterion, a
	// version with no such sub-command, a wrapper that swallowed it. That is a
	// HOST failure, not a script refusal: a caller that read it as a refusal
	// would hand the model a verdict about a broken rollout.
	return Response{}, fmt.Errorf(
		"scripthost: %s produced no response document and no bound explains it (wait: %v): %s",
		Subcommand, e.waitErr, evidence)
}

// burnedCPU is the CPU the child actually consumed, user plus system.
func burnedCPU(cmd *exec.Cmd) time.Duration {
	if cmd == nil || cmd.ProcessState == nil {
		return 0
	}
	return cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
}

// deathSignal reports whether the child was killed by a signal, and which.
func deathSignal(cmd *exec.Cmd) (bool, syscall.Signal) {
	if cmd == nil || cmd.ProcessState == nil {
		return false, 0
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false, 0
	}
	return true, ws.Signal()
}

// command builds the child. It is the ONE place a script-host process is
// constructed, which is what makes "the child holds no credential" a property
// of the code rather than of a convention — and what lets a test assert it
// against the real constructor instead of a double.
func command(ctx context.Context, binary string, payload []byte, stdout, stderr io.Writer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, Subcommand)

	// An EMPTY environment, not a filtered one. A provider key lives in this
	// process's environment on purpose — a run spends it — and an allowlist
	// would need widening every time a new name appeared, with the failure
	// mode pointing the wrong way. Nothing the child does needs a variable:
	// the request arrives on stdin and every bound travels inside it.
	cmd.Env = []string{}

	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Its own process group, killed on cancellation, so a descendant cannot
	// outlive the deadline holding the pipes open. The ordinary path needs
	// [proc.KillProcessGroup] as well — see Run.
	proc.TerminateGroupOnCancel(cmd)
	// Last resort if the group kill does not land: stop waiting on the pipes.
	proc.SetWaitDelay(cmd, 5*time.Second)
	// The kernel kills the child if this process dies abruptly. Belt, not
	// braces: Go can deliver this signal early if the thread that forked
	// exits, so the bound that really stops an orphan is RLIMIT_CPU, which the
	// child applies to itself and no parent can lose.
	setParentDeathSignal(cmd)

	return cmd
}

// decodeResponse reads the child's single document.
//
// It takes the FIRST document and ignores a trailing byte, because a stray
// byte on the stream used to discard a complete verdict: a descendant that
// wrote one character to the inherited stdout turned `{"value":"42"}` into
// "produced no response". The child refuses to WRITE a second document, so
// first-wins costs nothing and saves the answer.
//
// A document carrying neither a value nor a refusal is NOT a response: the
// real child never emits one (a script returning undefined yields `"null"`),
// and accepting `{}` as success would hand the model an empty tool result
// labelled OK.
func decodeResponse(out []byte) (Response, bool) {
	if len(bytes.TrimSpace(out)) == 0 {
		return Response{}, false
	}
	var resp Response
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&resp); err != nil {
		return Response{}, false
	}
	// A document carrying only a confinement report is not a verdict.
	if resp.Value == "" && resp.Refusal == "" {
		return Response{}, false
	}
	return resp, true
}

// looksLikeMemoryDeath matches what the Go runtime prints when an allocation
// fails under RLIMIT_DATA. Three spellings, all observed: the mmap path, the
// heap-growth path and the arena path.
func looksLikeMemoryDeath(stderrText string) bool {
	for _, marker := range []string{
		"runtime: out of memory",
		"runtime: cannot allocate memory",
		"out of memory allocating heap arena",
		// A cgo-enabled build charges thread stacks to RLIMIT_DATA too, and
		// the runtime's spelling for that one is different. The child pins
		// GOMAXPROCS to 1 so it should not happen, but a memory death
		// reported as `script_host_died` is the wrong verdict either way.
		"pthread_create failed",
	} {
		if strings.Contains(stderrText, marker) {
			return true
		}
	}
	return false
}

// looksLikeRuntimeCrash matches what the Go runtime prints when it dies of
// something it cannot report as an error: a fatal, a panic, or a signal it
// caught and dumped. It says "the child was iterion and it crashed", which is
// a different thing from "the deployment answered something else".
func looksLikeRuntimeCrash(stderrText string) bool {
	for _, marker := range []string{"fatal error:", "SIGSEGV:", "SIGBUS:", "SIGABRT:", "panic: ", "goroutine "} {
		if strings.Contains(stderrText, marker) {
			return true
		}
	}
	return false
}

// firstLines keeps the head of a multi-line blob. A Go fatal error puts the
// cause on its first line and every goroutine's stack after it.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// clip bounds a blob in BYTES. firstLines alone does not: 4096 newline-free
// bytes are one line, and they travelled whole into a refusal message.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// boundedWriter keeps the first limit bytes, records that it cut, and drops
// the rest. It never errors: a writer that failed mid-stack would make os/exec
// report a pipe error instead of the child's real death.
type boundedWriter struct {
	to    *bytes.Buffer
	limit int
	cut   bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	room := w.limit - w.to.Len()
	switch {
	case room <= 0:
		w.cut = w.cut || len(p) > 0
	case len(p) > room:
		w.to.Write(p[:room])
		w.cut = true
	default:
		w.to.Write(p)
	}
	return len(p), nil
}

// setParentDeathSignal asks the kernel to kill the child when this process
// dies. Linux-only; elsewhere the cancellation path and the child's own CPU
// bound are what stop an orphan.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	applyParentDeathSignal(cmd.SysProcAttr)
}
