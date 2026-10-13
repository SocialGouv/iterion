package toolcatalog

import "strings"

// ClawRefusal explains a name claw REGISTERS and iterion cannot serve: the
// tool is in the registry — so it resolves, and C135 has nothing to say — and
// every call earns a typed capability refusal instead of an answer.
//
// It is the half of the backend-parity rule that is not wiring: a capability
// wired for one backend is wired, or typed-refused, for the other — never
// simulated.
type ClawRefusal struct {
	// Reason is why the capability cannot be served, in the present tense.
	Reason string
	// Remedy is what to do instead, as an imperative the author and the model
	// can both act on.
	Remedy string
}

// String renders the pair as the one sentence both readers get.
func (r ClawRefusal) String() string { return r.Reason + ". " + r.Remedy }

// ClawRefusedToolHeadline opens the advertised description of a refused claw
// built-in, and is the phrase the ultracode orchestration grant points at when
// it tells the model what a refusal means and what to do instead
// (delegate.ultracodeOrchestrationInstruction). One constant for both: a
// reword on either side would otherwise leave the system prompt pointing at
// words no tool says.
const ClawRefusedToolHeadline = "UNAVAILABLE on this backend"

// clawRefusedBuiltins is that list. One source, two readers: pkg/dsl/ir warns
// at compile time (C159) and pkg/backend/tool refuses at run time. A name that
// stayed here after its capability became real would warn about a tool that
// works, so the conformance test in pkg/backend/tool executes every entry
// against the real registry and fails unless it still refuses.
var clawRefusedBuiltins = map[string]ClawRefusal{
	"agent": {
		Reason: "claw spawns a subagent only where the host wires a subagent runner into it " +
			"(ClawDefaults.Subagent, built by model.NewSubagentRunner), and iterion wires none: " +
			"claw's own executor reports a started subagent and launches nothing",
		Remedy: "Do the work in this node, or route it to claude_code, whose native Agent tool " +
			"(legacy name Task) spawns real subagents",
	},
}

// ClawRefusesBuiltin returns the refusal for a registered claw built-in, and
// whether there is one. A name outside the catalog never has one: the question
// only arises for a tool the registry holds.
func ClawRefusesBuiltin(name string) (ClawRefusal, bool) {
	r, refused := clawRefusedBuiltins[strings.TrimSpace(name)]
	return r, refused
}

// ClawRefusedBuiltins returns the refused names as a sorted slice — for the
// conformance test and for diagnostics. The returned slice is a copy.
func ClawRefusedBuiltins() []string {
	out := make([]string, 0, len(clawRefusedBuiltins))
	for name := range clawRefusedBuiltins {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}
