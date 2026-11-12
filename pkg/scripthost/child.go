package scripthost

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// epoch is the instant every script sees as "now". A fixed point rather than
// the real clock: see [newRuntime].
var epoch = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// RunChild is the child process. It reads one [Request] from in, confines
// itself, runs the script and writes one [Response] to out.
//
// The order is the whole point. The request is read BEFORE confinement because
// a deny-all Landlock ruleset does not revoke an already-open descriptor but
// would stop anything that still needed to open one; the interpreter is built
// AFTER it, so a defect in goja — or in a host binding a later change adds —
// meets a kernel that already refused.
//
// A refusal is a Response, written and exited zero: the caller distinguishes
// "the script was refused" from "the host died" by whether a document arrived
// at all. The only non-nil error RunChild returns is a failure of that
// protocol itself.
func RunChild(in io.Reader, out io.Writer) error {
	// One OS thread. RLIMIT_DATA covers private anonymous mappings, so every
	// thread stack and arena the runtime creates competes with the script's
	// heap — and the demand scales with the host's core count, which would
	// make the same bound mean different things on different nodes (philosophy
	// 3 forbids that). Measured on a cgo-enabled build on a 32-core host: a
	// 1.2-second script under the 256 MiB default died with
	// `pthread_create failed` at GOMAXPROCS=32 and returned its value at 16.
	// A script host is single-threaded by construction, so there is nothing to
	// lose here.
	runtime.GOMAXPROCS(1)

	// Parse-time and JSON.parse recursion consume the GO stack, which
	// MaxCallStack does not bound. Choosing the ceiling here means the child
	// dies at a bound this package set rather than at the host's default.
	debug.SetMaxStack(MaxGoStackBytes)

	req, err := readRequest(in)
	if err != nil {
		return err
	}

	limits, _ := req.Limits.Clamp()

	// The child enforces the script bound too. The parent refuses an oversized
	// script before spawning, which makes this unreachable through Host — and
	// "every bound travels inside the request, where the child re-clamps it"
	// was false all the same: Clamp re-clamps the limit VALUES, and nothing
	// compared the script's length against them. A hidden sub-command's
	// promises are part of its contract.
	if int64(len(req.Script)) > limits.ScriptBytes {
		return writeResponse(out, Refuse(RefusalScriptBytes,
			"the script is %d bytes, over the %d-byte bound", len(req.Script), limits.ScriptBytes))
	}

	applied, err := Confine(limits)
	if err != nil {
		// Confinement failing is the one case where the child must not run
		// the script: an unconfined script host is the thing this package
		// exists to prevent. It reports a refusal rather than dying, so the
		// parent can tell "cannot confine here" from "died for some reason".
		if errors.Is(err, ErrConfinementUnsupported) {
			return writeResponse(out, Refuse(RefusalUnavailable,
				"codemode needs a Linux kernel for its bounds (RLIMIT_DATA, RLIMIT_CPU, Landlock); this host offers none"))
		}
		return writeResponse(out, Refuse(RefusalUnavailable, "the script host could not confine itself: %v", err))
	}

	if refusal := confinementVerdict(applied, req); refusal != nil {
		refusal.Confinement = &applied
		return writeResponse(out, *refusal)
	}

	seed, err := resolveSeed(req.Seed)
	if err != nil {
		return err
	}

	vm := newRuntime(seed, epoch)

	// The binding goes in AFTER the confinement, like the interpreter: a
	// defect in it then meets a kernel that already refused. The descriptors
	// it wraps were opened by the SPAWN, before Confine, which is why a
	// deny-all ruleset leaves them usable — it stops opening, it does not
	// revoke.
	if req.ToolChannel {
		if req.Proto != Proto {
			return writeResponse(out, Refuse(RefusalUnavailable,
				"the tool channel speaks protocol %d and this script host speaks %d: "+
					"the parent and the child are not the same build", req.Proto, Proto))
		}
		bridge := newToolBridge(os.NewFile(fdToolReplies, "codemode-replies"),
			os.NewFile(fdToolCalls, "codemode-calls"), limits)
		if err := bridge.install(vm); err != nil {
			return writeResponse(out, Refuse(RefusalUnavailable,
				"the tool binding could not be installed: %v", err))
		}
	}

	resp := runScript(vm, req.Script, limits.Wall, limits.ResultBytes)
	resp.Confinement = &applied
	return writeResponse(out, resp)
}

// confinementVerdict is the ONE place that decides whether a posture is good
// enough to run a script in. Two ways it is not, and both are policy questions
// rather than syscall results — Confine reports them instead of failing:
//
//   - no Landlock at all: an unrestricted filesystem and unrestricted sockets;
//   - one thread confined instead of the process: the same, on every thread
//     but the one that applied the bounds.
//
// One function rather than a guard per condition, because the next posture
// would otherwise be checked at one site out of two. Separated from RunChild
// so the decision has a witness at all: where the kernel and the build are
// both healthy, every refusing branch here is unreachable through the real
// syscalls, and an unreachable branch is an untested one.
func confinementVerdict(applied Applied, req Request) *Response {
	switch {
	case applied.LandlockABI == 0 && !req.AllowWithoutLandlock:
		refusal := Refuse(RefusalUnavailable,
			"this kernel offers no Landlock, so a script would run with an unrestricted filesystem and "+
				"unrestricted sockets; the CPU and memory bounds alone are not what codemode promises. "+
				"Set allow_without_landlock on the request to accept that posture deliberately")
		return &refusal
	case !applied.AllThreads && !req.AllowSingleThreadConfinement:
		refusal := Refuse(RefusalUnavailable,
			"this binary could confine only the thread that asked (a cgo-linked build cannot drive its "+
				"own threads), so the filesystem, socket and exec bounds hold on one thread and not on "+
				"the process. Build with CGO_ENABLED=0, as iterion ships, or set "+
				"allow_single_thread_confinement to accept that posture deliberately")
		return &refusal
	default:
		return nil
	}
}

// resolveSeed takes the caller's seed when it sent one — which is what makes a
// run replayable, since the caller is the only party that can store it.
func resolveSeed(hexSeed string) ([32]byte, error) {
	var seed [32]byte
	if hexSeed != "" {
		raw, err := hex.DecodeString(hexSeed)
		if err != nil {
			return seed, fmt.Errorf("scripthost: the request's seed is not hex: %w", err)
		}
		if len(raw) != len(seed) {
			return seed, fmt.Errorf("scripthost: the request's seed is %d bytes, want %d", len(raw), len(seed))
		}
		copy(seed[:], raw)
		return seed, nil
	}
	if _, err := rand.Read(seed[:]); err != nil {
		// crypto/rand on Linux is getrandom(2), which Landlock does not
		// govern — a failure here is a broken kernel, not a bound.
		return seed, fmt.Errorf("scripthost: seeding the script's PRNG: %w", err)
	}
	return seed, nil
}

// MaxRequestBytes bounds what the child will read before it is confined.
//
// The read happens BEFORE Confine — it has to, because a deny-all ruleset
// would stop anything that still needed to open a descriptor — so no rlimit is
// in force for it. The ceiling's script bound plus room for the limits object
// is the honest cap.
const MaxRequestBytes = 4<<20 + 64<<10

// readRequest decodes the single request document.
//
// It refuses trailing content rather than ignoring it: a second document on
// this pipe means the caller believes it is speaking a streaming protocol this
// child does not implement, and silently running only the first script would
// hide that disagreement until something subtler broke.
//
// The check reads what is ALREADY buffered instead of calling Decoder.More(),
// which refills from the reader and therefore blocks for as long as a writer
// holds the pipe open — measured at 8 s for a producer that wrote the document
// and then slept. Production is safe either way (os/exec closes a
// bytes.Reader's pipe), but a child that waits on an idle pipe before it is
// confined is a bound nobody set.
func readRequest(in io.Reader) (Request, error) {
	dec := json.NewDecoder(io.LimitReader(in, MaxRequestBytes))

	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("scripthost: reading the request document: %w", err)
	}
	if rest, err := io.ReadAll(io.LimitReader(dec.Buffered(), 64)); err == nil && hasContent(rest) {
		return Request{}, errors.New("scripthost: more than one document on stdin — this child answers exactly one request and does not stream")
	}
	return req, nil
}

// hasContent reports whether b holds anything but JSON whitespace.
func hasContent(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			return true
		}
	}
	return false
}

// writeResponse emits the single response document.
func writeResponse(out io.Writer, resp Response) error {
	encoded, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("scripthost: encoding the response: %w", err)
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("scripthost: writing the response: %w", err)
	}
	return nil
}
