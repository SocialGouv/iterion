// Package scripthost runs model-authored JavaScript in a child process that
// holds no credential and no capability.
//
// This is the confinement half of codemode ([ADR-165]): the host, its bounds
// and the witnesses that the bounds bite. There is no tool binding here on
// purpose — nothing a node declares is reachable from a script, so the whole
// content of this package is what a compromised script CANNOT do. The binding
// that makes it useful arrives behind
// [github.com/SocialGouv/iterion/pkg/backend/model] tool dispatcher, in its
// own change.
//
// Three measurements shape the design, and each contradicts something the
// record assumed:
//
//   - A bare [goja.Runtime] exposes no `require`, no `console` and no timers:
//     those come from goja_nodejs's event loop, which this package does not
//     use. Determinism and the absence of a filesystem read primitive are
//     therefore properties of construction, not of a deny list.
//   - RLIMIT_AS cannot express a script's heap budget. A Go child dies at
//     startup ("runtime: out of memory") below roughly 2 GiB of address space,
//     because the runtime reserves far more virtual memory than it commits.
//     RLIMIT_DATA is the knob that works: a child starts under 64 MiB and a
//     400 MiB allocation inside the VM dies.
//   - RLIMIT_CPU is the only thing that stops a native regex bomb. goja's
//     own interrupt does not: measured here, a backreference regex ran past
//     60 s of wall clock with an interrupt posted at 1 s, while RLIMIT_CPU=2s
//     killed the same script. That is ADR-098's F19, reproduced on the engine
//     this tree pins.
//
// [ADR-165]: ../../docs/adr/165-codemode-runs-the-models-code-in-a-child-process-behind-the-tool-door.md
package scripthost

import (
	"errors"
	"fmt"
	"time"
)

// Subcommand is the hidden iterion sub-command that IS the child. It follows
// the ADR-029 convention (a double-underscore command, hidden from help) and
// nothing else of it: the claw runner's NDJSON multiplexing exists to
// correlate concurrent tool calls, and a script host with no tool binding has
// exactly one request and one response.
const Subcommand = "__script-host"

// Refusal codes. A refusal is a VERDICT the caller can branch on, never a
// free-text error: the bound that bit has to be greppable in a run's events.
const (
	// RefusalUnavailable: the platform cannot confine a child to the standard
	// this package promises, so no script runs. Both shapes of it — a kernel
	// with no Landlock, and a binary that can only confine one thread — are
	// postures rather than errors, which is why they are a refusal a caller
	// branches on and each has a greppable allowance on the request. Philosophy 1 keeps the escape hatch greppable rather than
	// silent; philosophy 3 makes "unavailable" a typed answer, not a fallback
	// to running unconfined.
	RefusalUnavailable = "codemode_unavailable"

	// RefusalScriptBytes: the source exceeded [Limits.ScriptBytes]. Refused
	// before the child is spawned, so an oversized script costs no process.
	RefusalScriptBytes = "script_too_large"

	// RefusalResultBytes: the value the script returned exceeded
	// [Limits.ResultBytes] once encoded. Refused child-side, so the oversized
	// payload never crosses the pipe.
	RefusalResultBytes = "result_too_large"

	// RefusalWallClock: the child outlived [Limits.Wall].
	RefusalWallClock = "wall_clock_exceeded"

	// RefusalCPUTime: the child was killed on [Limits.CPU]. This is the
	// native-regex class: the only refusal the interpreter cannot produce
	// itself, which is why it is read from the child's death and not from its
	// output.
	RefusalCPUTime = "cpu_time_exceeded"

	// RefusalMemory: the child died allocating past [Limits.Memory].
	RefusalMemory = "memory_exceeded"

	// RefusalHostDied: the child produced no response and its death names no
	// bound. Deliberately distinct from the three above: naming a bound we did
	// not observe would turn a host bug into a plausible-looking verdict.
	RefusalHostDied = "script_host_died"

	// RefusalScriptError: the script threw, or did not parse. The script's own
	// fault, reported with its message — not a bound.
	RefusalScriptError = "script_error"

	// RefusalCallStack: the script recursed past [MaxCallStack]. Its own code
	// rather than a bound on resources, and distinct from RefusalScriptError
	// all the same: goja reports a stack overflow as an error whose message is
	// a bare stack trace with no sentence in it, so without this code the
	// model would receive " at f (<eval>:1:24(10))" and no cause. A reader who
	// sees this code in a run's events is seeing a recursion bound, which is a
	// different conversation from a script that threw.
	RefusalCallStack = "call_stack_exceeded"
)

// ErrConfinementUnsupported is returned by Confine where the platform offers
// no way to bound the child. It carries [RefusalUnavailable].
var ErrConfinementUnsupported = errors.New("scripthost: this platform cannot confine a script host child")

// Limits are the bounds a script runs under. Every field has a platform
// ceiling in [Ceiling] that a tenant cannot raise, and a default in
// [DefaultLimits] that an operator can lower or raise up to that ceiling —
// philosophy 1 forbids a bound with no override, cloud multi-tenancy forbids
// an override with no ceiling.
type Limits struct {
	// Wall is how long the child may live, enforced by the parent. It is the
	// backstop for everything the child cannot report, including a child that
	// never starts.
	//
	// The wire name says NANOSECONDS because that is what time.Duration
	// encodes to. It read `wall_ms` once, while the three byte fields really
	// are in their named unit, so a hand-written request got a bound a
	// million times tighter than it asked for — `"wall_ms":30000` was
	// enforced as 30µs.
	Wall time.Duration `json:"wall_ns"`

	// CPU is the child's CPU-seconds ceiling, enforced by the kernel
	// (RLIMIT_CPU). Rounded UP to a whole second, because the limit's unit is
	// seconds and rounding down would silently tighten an operator's choice.
	CPU time.Duration `json:"cpu_ns"`

	// Memory bounds the child's data segment (RLIMIT_DATA), which is where a
	// Go heap grows. NOT RLIMIT_AS: see the package comment.
	Memory int64 `json:"memory_bytes"`

	// ScriptBytes caps the source. Enforced parent-side before the spawn.
	ScriptBytes int64 `json:"script_bytes"`

	// ResultBytes caps the encoded value the script returns. Enforced
	// child-side, so an oversized result is refused where it is produced.
	ResultBytes int64 `json:"result_bytes"`

	// ToolCalls caps how many of the node's tools one script may call.
	//
	// Enforced PARENT-side, by the thing that cannot be lied to. The child
	// counts as well, to refuse the next call without a round trip, but a
	// child is what is being bounded: a child that under-reported its count
	// would be served the budget it claimed. See serveToolCalls.
	//
	// It is not only a cost bound. Every dispatch fires the node's PreToolUse
	// hooks — a subprocess each, where the node declares one — and may run its
	// tool policy through a model-consulting checker, so a thousand calls is a
	// thousand spawns and up to a thousand classifier calls. The number is
	// what an operator meets as latency and spend long before it is a refusal.
	ToolCalls int64 `json:"tool_calls"`

	// ToolResultBytes caps ONE tool result handed back to a script, cut on a
	// rune boundary and flagged as cut.
	//
	// Separate from ResultBytes, which bounds what the script RETURNS: the
	// point of codemode is that a script may read far more than it reports, so
	// the two bounds answer different questions and a single number would make
	// one of them wrong.
	ToolResultBytes int64 `json:"tool_result_bytes"`
}

// DefaultLimits is what a script runs under when nobody chose. The memory
// floor is deliberate: 64 MiB of RLIMIT_DATA was measured to start a Go child
// and to kill a 400 MiB allocation, so the default is not a guess about what
// fits — it is above the floor that was shown to work.
func DefaultLimits() Limits {
	return Limits{
		Wall:        30 * time.Second,
		CPU:         10 * time.Second,
		Memory:      256 << 20,
		ScriptBytes: 256 << 10,
		ResultBytes: 1 << 20,
		// Sixty-four calls is what a loop over a page of results costs, and
		// well under where the per-dispatch hook and policy cost becomes the
		// thing an operator notices.
		ToolCalls:       64,
		ToolResultBytes: 64 << 10,
	}
}

// Ceiling is the platform bound a tenant cannot raise. The precedent is
// applyCloudBudgetCeiling in pkg/runner: a value a bundle may set, clamped by
// a value only the deployment may set.
func Ceiling() Limits {
	return Limits{
		Wall:            10 * time.Minute,
		CPU:             5 * time.Minute,
		Memory:          2 << 30,
		ScriptBytes:     4 << 20,
		ResultBytes:     8 << 20,
		ToolCalls:       4096,
		ToolResultBytes: 1 << 20,
	}
}

// Clamp returns l with every field inside [Ceiling], and every unset (zero or
// negative) field taken from [DefaultLimits]. The returned bool reports
// whether anything was clamped DOWN — a caller that asked for more than the
// platform allows is warned (C1xx), never silently served something else.
func (l Limits) Clamp() (Limits, bool) {
	def, ceil := DefaultLimits(), Ceiling()
	clamped := false

	clampDur := func(v, d, c time.Duration) time.Duration {
		if v <= 0 {
			return d
		}
		if v > c {
			clamped = true
			return c
		}
		return v
	}
	clampInt := func(v, d, c int64) int64 {
		if v <= 0 {
			return d
		}
		if v > c {
			clamped = true
			return c
		}
		return v
	}

	return Limits{
		Wall:        clampDur(l.Wall, def.Wall, ceil.Wall),
		CPU:         clampDur(l.CPU, def.CPU, ceil.CPU),
		Memory:      clampInt(l.Memory, def.Memory, ceil.Memory),
		ScriptBytes: clampInt(l.ScriptBytes, def.ScriptBytes, ceil.ScriptBytes),
		ResultBytes: clampInt(l.ResultBytes, def.ResultBytes, ceil.ResultBytes),

		ToolCalls:       clampInt(l.ToolCalls, def.ToolCalls, ceil.ToolCalls),
		ToolResultBytes: clampInt(l.ToolResultBytes, def.ToolResultBytes, ceil.ToolResultBytes),
	}, clamped
}

// CPUSeconds is RLIMIT_CPU's unit. Rounded up, never down: a 500 ms request
// becomes one second rather than zero, because zero means "kill on the first
// tick" and nobody asking for half a second means that.
func (l Limits) CPUSeconds() uint64 {
	if l.CPU <= 0 {
		return 0
	}
	secs := int64(l.CPU / time.Second)
	if l.CPU%time.Second != 0 {
		secs++
	}
	return uint64(secs)
}

// Applied is what [Confine] put in place, which is not always what was asked
// for: a deployment that hardens the runner with its own rlimits wins, because
// a stricter bound is the operator's choice and tightening is always safe.
type Applied struct {
	// LandlockABI is the ruleset's ABI version, or 0 where the kernel offers
	// no Landlock — in which case there is no filesystem and no socket bound.
	LandlockABI int `json:"landlock_abi"`

	// AllThreads says the per-thread bounds (no_new_privs and the Landlock
	// domain) were applied to every OS thread of the process, which is the
	// only form in which they bound the CHILD rather than one goroutine's
	// carrier. False means a cgo-linked binary refused the all-threads path:
	// the filesystem, socket and exec bounds then hold on one thread only,
	// and that is not what codemode promises.
	AllThreads bool `json:"all_threads"`

	// MemoryBytes and CPUSeconds are the values the kernel accepted.
	MemoryBytes uint64 `json:"memory_bytes"`
	CPUSeconds  uint64 `json:"cpu_seconds"`
}

// Request is the single JSON document the parent writes to the child's stdin.
// There is no framing: one document in, one document out. Multiplexing arrives
// with the tool binding that needs it, together with a second consumer for the
// framing seam — one consumer does not justify a protocol.
type Request struct {
	Script string `json:"script"`
	Limits Limits `json:"limits"`

	// Seed seeds the script's Math.random, hex-encoded, 32 bytes. The PARENT
	// draws it, so the parent holds it and a run can be replayed; a child that
	// drew its own would make "replayable" a word with nothing behind it.
	// Empty means the child draws one and the run is not replayable, which it
	// says rather than pretends.
	Seed string `json:"seed,omitempty"`

	// AllowWithoutLandlock lets a script run on a kernel that offers no
	// Landlock — no filesystem bound and no socket bound. It travels HERE and
	// not in the environment because the child's environment is deliberately
	// empty, so an env-var opt-in could never reach it. This is the greppable
	// escape hatch philosophy 1 requires of a load-bearing limit.
	AllowWithoutLandlock bool `json:"allow_without_landlock,omitempty"`

	// AllowSingleThreadConfinement lets a script run in a child that could
	// only confine ONE of its OS threads. A cgo-linked binary cannot drive the
	// others, so no_new_privs and the Landlock domain then cover the thread
	// that applied them and nothing else — measured: a sibling thread read,
	// wrote, connected and exec'd freely. iterion ships CGO_ENABLED=0, where
	// this never arises; the `-race` test leg needs cgo, which is the one
	// place it is set deliberately and on the record.
	AllowSingleThreadConfinement bool `json:"allow_single_thread_confinement,omitempty"`

	// ToolChannel says the parent opened the tool channel on descriptors 3 and
	// 4 and will serve calls on it. Without it the child installs no binding,
	// and a script that calls one gets a plain "call is not defined" — which is
	// the honest answer for a host that was not given a tool server.
	//
	// It travels in the request, like every other posture flag, because the
	// child's environment is empty by construction.
	ToolChannel bool `json:"tool_channel,omitempty"`

	// Proto is the tool-channel protocol version, meaningful only with
	// ToolChannel. A child that does not know the number refuses rather than
	// running a script whose capability it cannot serve: see [Proto] for the
	// half of the handshake this cannot cover.
	Proto int `json:"proto,omitempty"`
}

// Response is the single JSON document the child writes to its stdout. Exactly
// one of Value, Refusal is meaningful; Refusal always carries Message.
type Response struct {
	// Value is what the script's last expression evaluated to, JSON-encoded.
	Value string `json:"value,omitempty"`

	// Refusal is one of the Refusal* codes, empty on success.
	Refusal string `json:"refusal,omitempty"`

	// Message explains a refusal: the script's own error text, or what the
	// parent observed of a child that died.
	Message string `json:"message,omitempty"`

	// Confinement is what the child actually applied to itself. The CHILD is
	// the only party that knows, so it says — rather than leaving the parent
	// to infer a posture it cannot observe. A caller that cares whether the
	// bounds covered the process or one thread reads it here; it is absent
	// only when no child ran.
	Confinement *Applied `json:"confinement,omitempty"`
}

// Refuse builds a refusal response.
func Refuse(code, format string, args ...any) Response {
	return Response{Refusal: code, Message: fmt.Sprintf(format, args...)}
}

// OK reports whether the response carries a value rather than a refusal.
func (r Response) OK() bool { return r.Refusal == "" }
