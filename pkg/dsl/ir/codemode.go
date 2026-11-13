package ir

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
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

// ValidCodemode reports whether v is a value the compiler accepts. "" is
// valid and means inherit.
func ValidCodemode(v string) bool {
	return normCodemode(v) == "" || slices.Contains(CodemodeValues, normCodemode(v))
}

// The precedence rungs a resolved codemode can come from.
const (
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
// nearest first: the node's own value, then the workflow's, then the default.
//
// There is deliberately NO environment rung. ADR-132 §1 forbids the
// environment as a source for what a node is configured with, and ADR-165
// repeats it for this attribute: a switch an operator flips from a pod spec
// is a switch no `.bot` author can read.
//
// An explicit `off` on the node BEATS a workflow `on`, which is the whole
// reason the attribute is a string and not a bool: "" is inherit, and a bool
// cannot tell "off" from "unset".
func ResolveCodemode(node, workflow string) CodemodePolicy {
	if normCodemode(node) != "" {
		return CodemodePolicy{On: normCodemode(node) == "on", Source: CodemodeFromNode}
	}
	if normCodemode(workflow) != "" {
		return CodemodePolicy{On: normCodemode(workflow) == "on", Source: CodemodeFromWorkflow}
	}
	return CodemodePolicy{On: false, Source: CodemodeFromDefault}
}

// Declared reports whether the value was written by an author rather than
// inherited from the default. Refusing a default would make every node of a
// fleet fail the day a registry cell changes; refusing a declaration is the
// point.
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
// `ambient_context` is the second member of this class and is NOT screened
// either: a declared `workspace` on opencode is refused at dispatch and
// offered by the picker. The shared screen that would ask both questions at
// once is the right shape; this answers the one this slice introduced rather
// than changing another capability's launch behaviour in the same change.
func CodemodeOverrideRefusalReason(backend string, node LLMNode, wf *Workflow) string {
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
	policy := ResolveCodemode(node.GetCodemode(), wf.Codemode)
	if !policy.On || CodemodeServedBy(backend) {
		return ""
	}
	return fmt.Sprintf("this node has %s, and %s serves no codemode tool: the run is refused at dispatch. %s",
		policy, backend, CodemodeRemedy(backend))
}
