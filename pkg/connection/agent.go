package connection

import (
	"errors"
	"fmt"
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

// AgentGrantAdvice is the sentence an operator is owed the moment they grant a
// use nothing performs, or "" when the grant is honoured.
//
// Returned rather than logged: the surfaces that owe it (`connections add`,
// `connections list`) decide how to render a warning, and a package that
// authenticates calls has no business holding an operator's output stream.
func AgentGrantAdvice(caps []Capability) string {
	if AgentFacadeWired() {
		return ""
	}
	for _, c := range caps {
		if c == CapAgent {
			return "`agent` is granted and inert: no MCP facade reads it in this build, so this connection serves `action:` nodes only" +
				" (docs/adr/098-connector-catalog.md tracks the facade)"
		}
	}
	return ""
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
	// The operator whose connection holds ONLY `agent` is the one most likely
	// to read this and conclude the agent path is the one to take. It is not,
	// and saying so here is the difference between a refusal and a dead end.
	if e.agentGrantInert() {
		msg += "; and the `agent` grant it does hold is inert — no MCP facade reads it in this build"
	}
	return msg
}

// Unwrap exposes ErrAgentFacadeUnwired when the capability the connection
// actually holds is one nothing honours. Nil otherwise: a connection declaring
// a capability this build does serve is an ordinary grant mismatch.
func (e *CapabilityError) Unwrap() error {
	if e.agentGrantInert() {
		return ErrAgentFacadeUnwired
	}
	return nil
}

// agentGrantInert reports whether the ONLY thing this connection is granted is
// a use this build does not perform.
//
// One predicate for the message and for the unwrap. Two copies would let the
// sentence and the sentinel disagree, and the sentence is what an operator
// acts on while the sentinel is what code branches on.
func (e *CapabilityError) agentGrantInert() bool {
	if AgentFacadeWired() || len(e.Have) == 0 {
		return false
	}
	for _, c := range e.Have {
		if c != CapAgent {
			return false
		}
	}
	return true
}

// useOf names a capability as the USE it authorises, which is what an operator
// reading a refusal is looking for.
func useOf(c Capability) string {
	switch c {
	case CapAction:
		return "a deterministic node"
	case CapAgent:
		return "an agent through the MCP facade"
	}
	return string(c)
}
