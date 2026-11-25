package ir

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// Codemode (ADR-165) hands a node one extra tool whose input is a PROGRAM,
// run in a child process that holds no credential and whose only capability
// is to call the node's own tools, through the node's own dispatcher. A model
// that composes its calls as code keeps the loop, the intermediate results
// and the dead ends out of its context, and brings back what it distilled.
//
// The decision — which value a node ends up with, where it came from, and
// whether the backend it will run on serves the capability — lives HERE, with
// the field, and the compiler (C339) and the executor both ask it. A second
// spelling of this rule is how a compiler ends up accepting what the engine
// refuses.
//
// # Bounds (ADR-165 law 4)
//
// Two ceilings hold, and a `.bot` raises neither:
//
//   - the CAPABILITY ceiling is [CodemodeServedBy] — the backend registry,
//     compiled into the binary the platform deploys;
//   - the BOUNDS ceiling is scripthost.Ceiling, applied by Limits.Clamp in
//     the parent process, so a request for more memory, more CPU seconds or a
//     longer script is reduced where the program is spawned.
//
// Neither travels on the wire, so a stale consumer cannot drop either.

// CodemodeValues lists the canonical spellings, in the order the docs present
// them. The empty string is not among them: it is the ABSENCE of a
// declaration and means INHERIT, never off.
var CodemodeValues = []string{"on", "off"}

// CodemodeBounds is a declared bound set, and it IS [scripthost.Limits] —
// not a parallel struct that maps onto it.
//
// One type means one spelling and nothing to drift: the wire names
// (`wall_ns`, `memory_bytes`, …) are the ones the host enforces, so the
// `wall_ms` bug its own field doc records — a hand-written `"wall_ms":30000`
// enforced as 30µs — cannot be reintroduced by a second declaration of the
// same limits. It is also what the repository asks for in general:
// read the vocabulary where it is defined rather than keep a copy.
//
// A zero field means UNSET, which is exactly what Limits.Clamp already reads
// as "nobody chose" — so an author's `tool_calls: 0` is the default rather
// than a ban, and the resolution below can fill field by field without a
// third state to carry.
type CodemodeBounds = scripthost.Limits

// ResolveCodemodeBounds decides the bounds a node's programs run under, FIELD
// BY FIELD: the launch request's value where it set one, else the node's, else
// the workflow's, else the zero that Clamp fills from scripthost.DefaultLimits.
//
// Per-field rather than nearest-rung-wins, and that is a choice: the
// workflow rung is a DEFAULT — its own field doc says so — and a workflow
// that raises `tool_calls` for a whole bot would otherwise be erased by a
// node that only wanted a longer `wall`. The launch rung behaves the same
// way: an operator who was refused at a bound raises ONE bound ("ask the
// operator to raise the bound" names the one), and the rest of whatever the
// author chose stays. It is also the behaviour Clamp
// already has two rungs down, so the ladder is one rule applied three times
// rather than three rules that happen to meet.
//
// There is deliberately NO environment rung, for codemode's bounds as for its
// switch: ADR-132 §1, and a bound an operator flips from a pod spec is a bound
// no `.bot` author can read.
func ResolveCodemodeBounds(run, node, workflow *CodemodeBounds) *CodemodeBounds {
	if run == nil && node == nil && workflow == nil {
		return nil
	}
	pick := func(r, n, w int64) int64 {
		if r > 0 {
			return r
		}
		if n > 0 {
			return n
		}
		return w
	}
	pickDur := func(r, n, w time.Duration) time.Duration {
		if r > 0 {
			return r
		}
		if n > 0 {
			return n
		}
		return w
	}
	var r, n, w CodemodeBounds
	if run != nil {
		r = *run
	}
	if node != nil {
		n = *node
	}
	if workflow != nil {
		w = *workflow
	}
	out := CodemodeBounds{
		Wall:                pickDur(r.Wall, n.Wall, w.Wall),
		CPU:                 pickDur(r.CPU, n.CPU, w.CPU),
		Memory:              pick(r.Memory, n.Memory, w.Memory),
		ScriptBytes:         pick(r.ScriptBytes, n.ScriptBytes, w.ScriptBytes),
		ResultBytes:         pick(r.ResultBytes, n.ResultBytes, w.ResultBytes),
		ToolCalls:           pick(r.ToolCalls, n.ToolCalls, w.ToolCalls),
		ConcurrentToolCalls: pick(r.ConcurrentToolCalls, n.ConcurrentToolCalls, w.ConcurrentToolCalls),
		ToolResultBytes:     pick(r.ToolResultBytes, n.ToolResultBytes, w.ToolResultBytes),
	}
	return &out
}

// ResolveCodemodeStoreLimits resolves the quota of the one bank shared by a
// run, field by field. Only the launch and the workflow may choose it; a
// node never does. Zero inherits and the platform ceiling always applies.
func ResolveCodemodeStoreLimits(run, workflow *CodemodeBounds) (entries, bytes int64) {
	defaults, ceiling := scripthost.DefaultLimits(), scripthost.Ceiling()
	entries, bytes = defaults.StoreEntries, defaults.StoreBytes
	for _, rung := range []*CodemodeBounds{workflow, run} {
		if rung == nil {
			continue
		}
		if rung.StoreEntries > 0 {
			entries = rung.StoreEntries
		}
		if rung.StoreBytes > 0 {
			bytes = rung.StoreBytes
		}
	}
	return min(entries, ceiling.StoreEntries), min(bytes, ceiling.StoreBytes)
}

// CodemodeSubagents is the node-level sub-agent reserve, in the shape the
// task envelope carries (scripthost, which the delegate wire already imports;
// the alias keeps the IR's reading of it in one place, the way
// CodemodeBounds does).
type CodemodeSubagents = scripthost.SubagentReserve

// ResolveCodemodeSubagents decides the node's sub-agent reserve, field by
// field over the same three rungs ResolveCodemodeBounds walks. There is
// deliberately no run rung filled today — no launch surface carries the
// value — but the rung exists because a launch override is the same
// configuration kind, and the resolver is the shape a second bound beside
// the bounds must take. Declaring `subagents_tokens` at ANY rung arms the
// reserve for the node.
func ResolveCodemodeSubagents(run, node, workflow *CodemodeSubagents) *CodemodeSubagents {
	if run == nil && node == nil && workflow == nil {
		return nil
	}
	pick := func(r, n, w int64) int64 {
		if r > 0 {
			return r
		}
		if n > 0 {
			return n
		}
		return w
	}
	var r, n, w CodemodeSubagents
	if run != nil {
		r = *run
	}
	if node != nil {
		n = *node
	}
	if workflow != nil {
		w = *workflow
	}
	out := CodemodeSubagents{
		Tokens:         pick(r.Tokens, n.Tokens, w.Tokens),
		TokensPerAgent: pick(r.TokensPerAgent, n.TokensPerAgent, w.TokensPerAgent),
		Concurrent:     pick(r.Concurrent, n.Concurrent, w.Concurrent),
		StepsPerAgent:  pick(r.StepsPerAgent, n.StepsPerAgent, w.StepsPerAgent),
	}
	if out.Tokens <= 0 {
		// No rung sized the reserve: nothing is armed, and the per-field
		// defaults picked above bound nothing.
		return nil
	}
	return &out
}

// CodemodeSubagentsAboveCeiling names every field of the reserve above the
// platform ceiling, as "<property> <declared> > <ceiling>" phrases, the twin
// of CodemodeBoundsAboveCeiling for the reserve: the compile-time half of a
// bound the platform clamps anyway, warned rather than refused.
func CodemodeSubagentsAboveCeiling(b *CodemodeSubagents) []string {
	if b == nil {
		return nil
	}
	ceil := scripthost.SubagentCeiling()
	var over []string
	num := func(name string, v, c int64) {
		if v > c {
			over = append(over, fmt.Sprintf("%s %d > %d", name, v, c))
		}
	}
	num("subagents_tokens", b.Tokens, ceil.Tokens)
	num("subagents_tokens_per_agent", b.TokensPerAgent, ceil.TokensPerAgent)
	num("subagents_concurrent", b.Concurrent, ceil.Concurrent)
	num("subagents_steps_per_agent", b.StepsPerAgent, ceil.StepsPerAgent)
	return over
}

// ReplayCodemodeBounds keeps existing replacement semantics when a restatement
// includes a per-program bound. Restating only bank quotas leaves per-program
// bounds untouched, and each omitted quota keeps its previous value. The
// result is an execution snapshot, not an explicit quota patch for a claim.
func ReplayCodemodeBounds(restated, prior *CodemodeBounds) *CodemodeBounds {
	if restated == nil {
		return prior
	}
	if prior == nil {
		return restated
	}
	entries, bytes := restated.StoreEntries, restated.StoreBytes
	out := *restated
	out.StoreEntries, out.StoreBytes = 0, 0
	if out == (CodemodeBounds{}) && (entries > 0 || bytes > 0) {
		out = *prior
	}
	out.StoreEntries, out.StoreBytes = entries, bytes
	if out.StoreEntries == 0 {
		out.StoreEntries = prior.StoreEntries
	}
	if out.StoreBytes == 0 {
		out.StoreBytes = prior.StoreBytes
	}
	return &out
}

// CodemodeBoundsAboveCeiling names every field of b that is above the
// platform ceiling, as "<property> <declared> > <ceiling>" phrases in the
// order the DSL block lists them. Empty means nothing is above it.
//
// It is the compiler's half of a bound that is clamped anyway: the parent
// process reduces the value at spawn (Limits.Clamp) and reports it through
// OnClamp, but an author learns that at RUN time, on a run that already cost
// money. C347 moves the same fact to compile time. It WARNS rather than
// refuses — philosophy 1 — and the clamp remains what enforces it: a
// diagnostic that replaced the clamp would be a bound with two owners.
func CodemodeBoundsAboveCeiling(b *CodemodeBounds) []string {
	if b == nil {
		return nil
	}
	ceil := scripthost.Ceiling()
	var over []string
	dur := func(name string, v, c time.Duration) {
		if v > c {
			over = append(over, fmt.Sprintf("%s %s > %s", name, v, c))
		}
	}
	num := func(name string, v, c int64) {
		if v > c {
			over = append(over, fmt.Sprintf("%s %d > %d", name, v, c))
		}
	}
	num("tool_calls", b.ToolCalls, ceil.ToolCalls)
	num("concurrent_tool_calls", b.ConcurrentToolCalls, ceil.ConcurrentToolCalls)
	dur("wall", b.Wall, ceil.Wall)
	dur("cpu", b.CPU, ceil.CPU)
	num("memory", b.Memory, ceil.Memory)
	num("script_bytes", b.ScriptBytes, ceil.ScriptBytes)
	num("result_bytes", b.ResultBytes, ceil.ResultBytes)
	num("tool_result_bytes", b.ToolResultBytes, ceil.ToolResultBytes)
	num("store_entries", b.StoreEntries, ceil.StoreEntries)
	num("store_bytes", b.StoreBytes, ceil.StoreBytes)
	return over
}

// ValidCodemode reports whether v is a value the compiler accepts. "" is
// valid and means inherit. The launch rung accepts the same vocabulary: the
// CLI flag and the API field are validated with this, so a typo cannot read
// as "inherit" downstream of a surface that did not check.
func ValidCodemode(v string) bool {
	return normCodemode(v) == "" || slices.Contains(CodemodeValues, normCodemode(v))
}

// ValidCodemodeDefer is ValidCodemode for the defer property: the same two
// spellings, "" meaning inherit. It is the predicate the compiler judges the
// block's value with (C354), so a second surface that grows a launch rung
// validates with this rather than re-deriving the set.
func ValidCodemodeDefer(v string) bool {
	return normCodemode(v) == "" || slices.Contains(CodemodeValues, normCodemode(v))
}

// ResolveCodemodeDefer decides whether a node defers its own `tools:` list to
// the codemode catalogue, nearest rung first: the launch request's value,
// then the node's, then the workflow's, then "" — which reads as off.
//
// There is deliberately NO launch surface carrying the value yet (no queue
// field, no CLI flag — the queue's SchemaVersion was not bumped for it), so
// today every caller passes "" at the run rung. The rung exists because the
// attribute is a configuration the operator may one day override per run,
// and ResolveCodemode is the shape a second switch beside it must take —
// nearest-wins, "" inherit, no environment rung (ADR-132 §1).
//
// The two ladders are walked SEPARATELY, and that has one reach worth
// stating: a launch override of the MODE (ResolveCodemode's run rung) turns
// codemode on for a node whose own block says `mode: off` — and this
// resolver then answers that node's `defer: on` as written, so the deferral
// goes live with it. That is the parity C348 has for the bounds beside
// `mode: off`, and why C355 says "inert as written", never "inert".
func ResolveCodemodeDefer(run, node, workflow string) string {
	for _, v := range []string{normCodemode(run), normCodemode(node), normCodemode(workflow)} {
		if v != "" {
			return v
		}
	}
	return ""
}

// CodemodeBoundsSet reports whether the launch (or an author) set ANY field
// of b. It is the launch's half of C348: bounds beside a run-level
// `codemode: off` are inert, and the launch path warns rather than refuses —
// philosophy 1 — naming the compile-time twin in its message.
func CodemodeBoundsSet(b *CodemodeBounds) bool {
	return b != nil && (b.Wall > 0 || b.CPU > 0 || b.Memory > 0 ||
		b.ScriptBytes > 0 || b.ResultBytes > 0 ||
		b.ToolCalls > 0 || b.ConcurrentToolCalls > 0 || b.ToolResultBytes > 0 ||
		b.StoreEntries > 0 || b.StoreBytes > 0)
}

// CodemodeBoundsNegativeField names the first property of b set BELOW zero,
// or "". A negative bound is a typo, not a choice: the resolver's pick rule
// would silently read it as unset, which is the inheritance-without-saying
// the launch surface refuses everywhere else (a bad value is named, never
// silently inverted).
func CodemodeBoundsNegativeField(b *CodemodeBounds) string {
	if b == nil {
		return ""
	}
	checkInt := func(name string, v int64) string {
		if v < 0 {
			return name
		}
		return ""
	}
	checkDur := func(name string, v time.Duration) string {
		if v < 0 {
			return name
		}
		return ""
	}
	for _, name := range []string{
		checkInt("tool_calls", b.ToolCalls),
		checkInt("concurrent_tool_calls", b.ConcurrentToolCalls),
		checkDur("wall", b.Wall),
		checkDur("cpu", b.CPU),
		checkInt("memory", b.Memory),
		checkInt("script_bytes", b.ScriptBytes),
		checkInt("result_bytes", b.ResultBytes),
		checkInt("tool_result_bytes", b.ToolResultBytes),
		checkInt("store_entries", b.StoreEntries),
		checkInt("store_bytes", b.StoreBytes),
	} {
		if name != "" {
			return name
		}
	}
	return ""
}

// CodemodeOnSomewhere reports whether the AUTHORED ladder resolves codemode
// on for any node of wf (or the workflow rung itself). It is the launch
// warning's measure of the effective mode when the launch expressed none:
// bounds set for a workflow where no program can ever run are inert, and
// saying so at launch is the whole point of the warning.
func CodemodeOnSomewhere(wf *Workflow) bool {
	if wf == nil {
		return false
	}
	for _, node := range wf.Nodes {
		llm, ok := node.(LLMNode)
		if !ok {
			continue
		}
		if ResolveCodemode("", llm.GetCodemode(), wf.Codemode).On {
			return true
		}
	}
	return ResolveCodemode("", "", wf.Codemode).On
}

// The precedence rungs a resolved codemode can come from.
const (
	// CodemodeFromRun is the launch request: the operator's explicit choice
	// for THIS run (`--codemode` on the CLI, the API's codemode field). It is
	// not an environment rung — ADR-132 §1 forbids the environment as a
	// source for what a node is configured with, and this travels on the
	// launch request and the queue message, named and logged, never from a
	// pod spec.
	CodemodeFromRun = "launch"
	// CodemodeFromNode is the node's own `codemode:`.
	CodemodeFromNode = "node"
	// CodemodeFromWorkflow is the workflow-level `codemode:`, inherited by a
	// node that declares nothing.
	CodemodeFromWorkflow = "workflow"
	// CodemodeFromDefault is the absence of any declaration. The default is
	// OFF: codemode changes what a model is handed, so it is opted into.
	CodemodeFromDefault = "default"
)

// CodemodePolicy is a node's resolved codemode: the decision AND the rung it
// came from, carried together. The two are never reconstructed from each
// other — "on" alone cannot tell a declaration from a default, and only a
// declaration is refused on a backend that cannot serve it.
type CodemodePolicy struct {
	On     bool
	Source string
}

// ResolveCodemode decides a node's codemode from the rungs that may carry it,
// nearest first: the launch request's value (the run-level override, "" on
// every path that has no launch — the compiler passes it, because a compile
// sees no launch), then the node's own value, then the workflow's, then the
// default.
//
// There is deliberately NO environment rung. ADR-132 §1 forbids the
// environment as a source for what a node is configured with, and ADR-165
// repeats it for this attribute: a switch an operator flips from a pod spec
// is a switch no `.bot` author can read. The run-level override is a
// different thing entirely — an explicit choice on the launch request, named
// on the wire and in the run's own record.
//
// An explicit `off` on a nearer rung BEATS a farther `on`, which is the whole
// reason the attribute is a string and not a bool: "" is inherit, and a bool
// cannot tell "off" from "unset". The run rung wins in BOTH directions: an
// operator who launches a bot with `codemode: off` at run level gets a run
// without the capability even where the author declared it, and one who asks
// for it on a node whose author stayed silent gets it — with the default
// bounds, since the author set none.
func ResolveCodemode(run, node, workflow string) CodemodePolicy {
	if normCodemode(run) != "" {
		return CodemodePolicy{On: normCodemode(run) == "on", Source: CodemodeFromRun}
	}
	if normCodemode(node) != "" {
		return CodemodePolicy{On: normCodemode(node) == "on", Source: CodemodeFromNode}
	}
	if normCodemode(workflow) != "" {
		return CodemodePolicy{On: normCodemode(workflow) == "on", Source: CodemodeFromWorkflow}
	}
	return CodemodePolicy{On: false, Source: CodemodeFromDefault}
}

// Declared reports whether the value was written by an author or chosen on
// the launch request, rather than inherited from the default. Refusing a
// default would make every node of a fleet fail the day a registry cell
// changes; refusing a declaration is the point — and a launch override is a
// declaration, made by the operator who is about to pay for the run.
func (p CodemodePolicy) Declared() bool {
	return p.Source != "" && p.Source != CodemodeFromDefault
}

// String names the policy the way a refusal shows it:
// "codemode: on (from workflow)".
func (p CodemodePolicy) String() string {
	value := "off"
	if p.On {
		value = "on"
	}
	return fmt.Sprintf("codemode: %s (from %s)", value, p.Source)
}

// The registry answers TWO questions about a backend, and they are not each
// other's negation — a name the registry does not carry answers no to both.
// Collapsing them into one predicate is a defect in whichever direction it is
// collapsed, and an adversarial round found it collapsed the permissive way:
//
//   - [CodemodeServedBy] is the POSITIVE question, and it is what decides
//     whether a node is HANDED the tool. Only a cell that says shipped is a
//     yes, so an unregistered name — a custom backend in an operator's own
//     registry, which `delegate.Registry` deliberately allows — is refused
//     out loud at dispatch rather than silently running without the
//     capability its author declared.
//   - [CodemodeRefusedBy] is the question the COMPILER asks, and only a cell
//     that says typed-refused is a yes. That is what keeps the published
//     contract of the registry (docs/backend-capability-registry.md: "unknown
//     names acquire neither capabilities nor compiler refusals") — a `.bot`
//     naming a backend this build has never heard of is not refused at
//     compile for a capability nobody can answer for.
//
// The two therefore DISAGREE on exactly one class, deliberately: an
// unregistered name compiles and is refused at dispatch. Every name the
// registry does carry gets the same answer from both, which
// TestTheExecutorAndTheCompilerRefuseTheSameBackends walks the registry to
// hold.

// CodemodeServedBy reports whether backend hands a node the codemode tool.
func CodemodeServedBy(backend string) bool {
	return backendcaps.Verdict(backend, backendcaps.Codemode) == backendcaps.Shipped
}

// CodemodeRefusedBy reports whether the registry NAMES backend as one that
// refuses codemode — the compiler's question, asked the way
// validateAsyncBackends asks its own (C267).
func CodemodeRefusedBy(backend string) bool {
	return backendcaps.Verdict(backend, backendcaps.Codemode) == backendcaps.TypedRefused
}

// CodemodeServingBackends lists, sorted, the backends whose cell serves
// codemode. It is what a refusal offers instead of a dead end.
func CodemodeServingBackends() []string {
	var out []string
	for _, b := range backendcaps.Names() {
		if CodemodeServedBy(b) {
			out = append(out, b)
		}
	}
	slices.Sort(out)
	return out
}

// CodemodeRemedy is what a refusal on backend tells the operator: the ways
// out, in the order they cost.
func CodemodeRemedy(backend string) string {
	serving := CodemodeServingBackends()
	if len(serving) == 0 {
		// Unreachable while the registry ships claw's cell, and written
		// rather than assumed: a remedy naming an empty list reads as a bug
		// in the message, not as the truth about the build.
		return fmt.Sprintf("no backend in this build serves codemode; drop the declaration, or declare `codemode: off` on this node to keep it off a workflow default (ADR-165) — backend %q", backend)
	}
	return fmt.Sprintf("declare `codemode: off` on this node, route it to a backend that serves it (%s), or drop the declaration — ADR-165",
		strings.Join(serving, ", "))
}

// CodemodeOverrideRefusalReason is why a LAUNCH OVERRIDE may not route this
// node to backend, or "" when it may. It is the picker's half of C339.
//
// A launch override wins over everything the source wrote
// (resolveBackendName returns it first), so it reaches a route the compiler
// never saw and C339 cannot have judged. The launch preview is then the only
// screen between an operator and a dispatch that raises
// ErrCapabilityUnsupported — and its own contract says the picker "cannot
// drift from runtime admission".
//
// `run` is the launch request's codemode choice, the same rung ResolveCodemode
// reads nearest: the screen must answer what the DISPATCH will answer, and
// with a run-level mode override the dispatch grants on the operator's choice
// too — an explicit `--codemode on` on a node whose author wrote nothing, or
// an explicit `--codemode off` that keeps the tool away. Empty inherits the
// authored ladder, the behaviour every caller before the override existed.
//
// `ambient_context` is the second member of this class and is NOT screened
// either: a declared `workspace` on opencode is refused at dispatch and
// offered by the picker. The shared screen that would ask both questions at
// once is the right shape; this answers the one this slice introduced rather
// than changing another capability's launch behaviour in the same change.
func CodemodeOverrideRefusalReason(backend, run string, node LLMNode, wf *Workflow) string {
	if node == nil || wf == nil {
		return ""
	}
	// The DISPATCH'S question, not the compiler's. This screen stands between
	// an operator and a run, so it must answer what the executor will answer:
	// a grant needs a `shipped` cell. Asking CodemodeRefusedBy here — as the
	// first draft did — offered every name the registry does not carry, which
	// the dispatch then refuses, and that is the drift this screen exists to
	// prevent. The compiler keeps its own, gap-tolerant question, for the
	// published-contract reason stated above.
	policy := ResolveCodemode(run, node.GetCodemode(), wf.Codemode)
	if !policy.On || CodemodeServedBy(backend) {
		return ""
	}
	return fmt.Sprintf("this node has %s, and %s serves no codemode tool: the run is refused at dispatch. %s",
		policy, backend, CodemodeRemedy(backend))
}
