package connection

import (
	"errors"
	"fmt"
	"strings"
)

// The `agent` capability, and the one place this build's answer about it
// lives (ADR-098, docs/adr/098-connector-catalog.md).
//
// CapAgent names a use NOTHING here performs: the MCP facade through which a
// model would reach a connector operation is not written, and no caller
// anywhere resolves a connection for an agent. An operator can nevertheless
// grant it — `connections add --capability agent` writes it, Validate accepts
// it, the store keeps it, `connections list` printed it — so the grant reads
// as a guarantee and is a no-op.
//
// That is precisely the failure mode this package's doc names as designed out
// ("a capability nobody checks is configuration that looks like a
// guarantee"), and the second capability was the one it happened to. It is
// answered here rather than fixed by removing the capability: `agent` is a
// settled decision (Q1-Q8 of 2026-09-10, three tiers and an MCP facade from
// the same package), the grant an operator wrote is theirs, and withdrawing
// it silently would be the same defect wearing the opposite sign.
//
// So the capability becomes READ: by the resolution chokepoint, which names
// what an agent-only grant does and does not buy today, and by the CLI, where
// the grant is created and listed. When the facade lands it flips here, in one
// place, and both readers tell the truth without being edited.
//
// `agent` was the first capability to need this and it is no longer the only
// one (`trigger`, trigger.go), so the machinery that answers "does this build
// perform it, and what is an operator owed if it does not" lives HERE for the
// whole set — one table, one advice, one predicate — while each capability
// keeps its own single source for the answer itself. A second copy per
// capability is how a surface comes to print the truth about one grant and
// nothing about the next.

// ErrAgentFacadeUnwired is what an `agent` grant is worth in this build.
//
// A SENTINEL, because the two answers a caller has to tell apart are not the
// same kind of fact: "this connection declares the wrong capability" is fixed
// on the record by its operator, while "the capability it declares is one this
// build cannot honour" is fixed by shipping the facade. A substring match on a
// message cannot carry that difference across a release.
var ErrAgentFacadeUnwired = errors.New("connection: the `agent` capability is granted but inert — no MCP facade reads it in this build (docs/adr/098-connector-catalog.md)")

// AgentFacadeWired reports whether this build can serve CapAgent.
//
// The single source of the answer. Every reader asks here, so the day the
// facade exists there is one line to change and no second site that still
// says otherwise.
func AgentFacadeWired() bool { return false }

// capabilityWired reports whether this build PERFORMS a capability.
//
// The one table every reader asks — the advice, the refusal's sentence and its
// sentinel — so a capability cannot be honoured by one of them and inert for
// another. Each arm delegates to that capability's own single source, which is
// what keeps "the day it lands there is one line to change" true per
// capability rather than per reader.
func capabilityWired(c Capability) bool {
	switch c {
	case CapAction:
		return true
	case CapAgent:
		return AgentFacadeWired()
	case CapTrigger:
		return TriggerIngressWired()
	}
	// Not a capability this build knows. Answering "wired" would be the
	// fail-open direction, and the callers below separate unknown from inert
	// themselves.
	return false
}

// unwiredSentinel is the typed fact a capability this build does not perform
// carries, or nil for one it does.
func unwiredSentinel(c Capability) error {
	switch c {
	case CapAgent:
		if !AgentFacadeWired() {
			return ErrAgentFacadeUnwired
		}
	case CapTrigger:
		if !TriggerIngressWired() {
			return ErrTriggerIngressUnwired
		}
	}
	return nil
}

// inertAdvice is the sentence an operator is owed for ONE granted-but-inert
// capability, or "" when this build performs it.
func inertAdvice(c Capability) string {
	switch c {
	case CapAgent:
		if !AgentFacadeWired() {
			return "`agent` is granted and inert: no MCP facade reads it in this build, so this connection serves `action:` nodes only" +
				" (docs/adr/098-connector-catalog.md tracks the facade)"
		}
	case CapTrigger:
		if !TriggerIngressWired() {
			return "`trigger` is granted and inert: no connector webhook ingress reads it in this build, so no delivery is believed on this connection's word yet" +
				" (docs/adr/098-connector-catalog.md tracks the inbound half)"
		}
	}
	return ""
}

// GrantAdvice is the sentence an operator is owed the moment they grant a use
// nothing performs, or "" when every grant is honoured.
//
// Returned rather than logged: the surfaces that owe it (`connections add`,
// `connections list`) decide how to render a warning, and a package that
// authenticates calls has no business holding an operator's output stream.
//
// ONE function for every capability rather than one per capability: the two
// surfaces that owe the sentence would otherwise have to remember a new call
// each time the set grows, and the third capability is the one that proved
// they would not.
func GrantAdvice(caps []Capability) string {
	var out []string
	for _, c := range caps {
		if a := inertAdvice(c); a != "" {
			out = append(out, a)
		}
	}
	return strings.Join(out, "; ")
}

// inertOnly reports the granted capabilities this build does not perform, and
// whether they are ALL of what the record holds.
//
// A capability this build does not RECOGNISE is not inert — it is unknown, and
// a record carrying one is an ordinary grant mismatch (Validate refuses such a
// record on the way in, so one that reaches here came from a newer replica or
// a hand-edited row). Keeping the two apart is the whole point of the
// sentinel: "the build cannot honour what you hold" is fixed by a release,
// "you granted the wrong thing" by its operator.
func inertOnly(caps []Capability) ([]Capability, bool) {
	if len(caps) == 0 {
		return nil, false
	}
	inert := make([]Capability, 0, len(caps))
	for _, c := range caps {
		if !ValidCapability(c) || capabilityWired(c) {
			return nil, false
		}
		inert = append(inert, c)
	}
	return inert, true
}

// CapabilityError is the resolution chokepoint's typed refusal of a
// connection that may not serve the use being resolved.
//
// Typed because the chokepoint's callers — a node executor, a future facade, a
// studio surface — have to distinguish a grant the operator can widen from a
// capability this build cannot honour at all, and because the refusal travels
// up through ResolveAction as "the node failed" otherwise.
type CapabilityError struct {
	// Alias is the connection as a `.bot` names it.
	Alias string
	// Want is the capability the use requires.
	Want Capability
	// Have is what the connection declares.
	Have []Capability
}

func (e *CapabilityError) Error() string {
	msg := fmt.Sprintf("connection %q may not serve %s: it declares %s, not %q — an operator grants that use deliberately",
		e.Alias, useOf(e.Want), describeCaps(e.Have), e.Want)
	// The operator whose connection holds ONLY uses this build does not
	// perform is the one most likely to read this and conclude one of them is
	// the path to take. None is, and saying so here is the difference between
	// a refusal and a dead end.
	if inert, only := inertOnly(e.Have); only {
		msg += "; and the grant it does hold is inert in this build: " + strings.Join(inertNames(inert), ", ")
	}
	return msg
}

// inertNames words the inert grants for the sentence above: the capability and
// what is missing, which is what tells an operator whether to wait for a
// release or to widen the record.
func inertNames(caps []Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		switch c {
		case CapAgent:
			out = append(out, "`agent` (no MCP facade reads it)")
		case CapTrigger:
			out = append(out, "`trigger` (no connector webhook ingress reads it)")
		default:
			out = append(out, string(c))
		}
	}
	return out
}

// Unwrap exposes the unwired sentinel of every capability the connection
// actually holds, when ALL of them are uses nothing honours. Nil otherwise: a
// connection declaring a capability this build does serve is an ordinary grant
// mismatch.
//
// SEVERAL inert grants join rather than pick: both facts are true, and a
// caller that received only the first would be reading the record's field
// ORDER as though it decided something. errors.Is matches either.
func (e *CapabilityError) Unwrap() error {
	inert, only := inertOnly(e.Have)
	if !only {
		return nil
	}
	errs := make([]error, 0, len(inert))
	for _, c := range inert {
		if err := unwiredSentinel(c); err != nil {
			errs = append(errs, err)
		}
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	return errors.Join(errs...)
}

// useOf names a capability as the USE it authorises, which is what an operator
// reading a refusal is looking for.
func useOf(c Capability) string {
	switch c {
	case CapAction:
		return "a deterministic node"
	case CapAgent:
		return "an agent through the MCP facade"
	case CapTrigger:
		return "an inbound delivery believed on this connection's word"
	}
	return string(c)
}
