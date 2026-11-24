package connection

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// The `agent` capability, and the one place this build's answer about it
// lives (ADR-098, docs/adr/098-connector-catalog.md).
//
// The facade EXISTS now (#2758): an agent node declaring
// `capabilities: [connectors]` is wired to the server's MCP facade endpoint
// over HTTP with a per-run token, the facade resolves the connection through
// Resolver.ResolveAgent — this package's chokepoint, requiring CapAgent — and
// the credential is spent at the server, never entering the sandbox. So
// AgentFacadeWired answers true, and both CLI surfaces
// (`connections add`, `connections list`) stopped warning because they read
// the one predicate, not a copy of it.
//
// The machinery below stays, because the failure mode it answers is
// permanent: a capability nobody checks is configuration that looks like a
// guarantee. `agent` was the first capability to need it and is no longer
// the only one (`trigger`, trigger.go, still unwired), so the table, the
// advice and the sentinels live HERE for the whole set — one predicate per
// capability, every reader asking the same question, so the day a build
// ships without a capability's surface there is one line to change and no
// second site that still says otherwise.

// CapConnectors is the DSL capability an agent or judge node declares to
// reach the MCP facade (`capabilities: [connectors]`): the workflow's half of
// an `agent` connection grant. The connection's own `agent` capability is the
// operator's half; a call needs both, which is what keeps a workflow from
// spending a credential its operator granted to automation only.
//
// The string lives HERE and not with the board capabilities because the two
// halves of the grant must be read together: this file is where the agent
// capability's answer lives.
const CapConnectors = "connectors"

// ErrAgentFacadeUnwired is what an `agent` grant is worth in a build with no
// facade. UNREACHABLE since #2758 (AgentFacadeWired answers true) and kept as
// the sentinel the predicate arms again the day a build ships without the
// facade — the capability table below reads it, and a substring match on a
// message could not carry "fixed by a release" versus "fixed on the record"
// across one.
var ErrAgentFacadeUnwired = errors.New("connection: the `agent` capability is granted but inert — no MCP facade reads it in this build (docs/adr/098-connector-catalog.md)")

// AgentFacadeWired reports whether this build can serve CapAgent.
//
// The single source of the answer. Every reader asks here, so the day the
// facade is missing there is one line to change and no second site that still
// says otherwise. TRUE since #2758: the facade is pkg/connector/facade, served
// at /api/v1/mcp/connectors (pkg/server/mcp_connector_routes.go), reached by
// an agent node declaring `capabilities: [connectors]`.
func AgentFacadeWired() bool { return true }

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
			// Unreachable while AgentFacadeWired answers true (the arm is the
			// sentence for the build where it flips back), kept so the table
			// stays the one place that words every capability.
			out = append(out, "`agent` (no agent facade is wired in this build)")
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

// MaturityError is the resolution chokepoint's refusal of an operation whose
// maturity promises nothing: `spotted`, or a level whose typo reads as it.
// Maturity is DATA THE SERVER ENFORCES — `Attachable` is written "may be
// bound to a bot OR USED BY A NODE" — and the node path already refused it by
// name (checkOperationUsable), so the chokepoint is where the rule holds for
// every door at once: the deterministic node, the agent's facade call, and
// whatever surface resolves through a Resolver next.
type MaturityError struct {
	// Operation is the id the caller asked to resolve.
	Operation string
	// Maturity is the EFFECTIVE level: an operation that declares none
	// inherits the package's, which is what a typo at the package level
	// silently did.
	Maturity spec.Maturity
	// Tier is the tier serving the package — the thing an operator repairs
	// (or waits on) to change the answer.
	Tier Tier
}

func (e *MaturityError) Error() string {
	return fmt.Sprintf("operation %q is at maturity %q, which promises nothing and may not be used by a node or an agent — qualify it in the package the %s serves",
		e.Operation, e.Maturity, e.Tier)
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
