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

// clawRefusedBuiltins is that list, and it is EMPTY: every name claw
// registers, iterion now serves. `agent` left it when the subagent runner was
// wired at both hosts — the in-process one (pkg/runview) and the in-container
// one (cmd/iterion), which is where tool.SandboxPlacementOf puts it (#1940).
// A name that stayed here after its capability became real would warn about a
// tool that works, which teaches authors to ignore diagnostics.
//
// The seam stays: one source, two readers — pkg/dsl/ir warns at compile time
// (C159) and pkg/backend/tool refuses at run time — because the parity rule
// needs it for the next capability claw ships before iterion serves it. With
// no entry, both readers are silent and their mechanisms are exercised
// through SetClawRefusedBuiltinForTest, which is the only way a guard can
// still see them fire.
//
// A deployment whose own host wires no runner is a different fact, decided at
// run time and answered by ClawUnwiredSubagentRefusal: this list is about what
// ITERION cannot serve, which is what a compiler can know.
var clawRefusedBuiltins = map[string]ClawRefusal{}

// ClawUnwiredSubagentRefusal is what a call to `agent` earns where the host
// that built the tool registry wired no subagent runner. iterion's own hosts
// wire one, so this answers the hosts that do not: a stand-alone embedding, a
// test registry, a future surface that forgets.
//
// It is deliberately NOT in clawRefusedBuiltins: the compiler cannot know
// which host will build the registry, and a C159 on every declared `agent`
// would warn about a capability the shipped hosts serve.
var ClawUnwiredSubagentRefusal = ClawRefusal{
	Reason: "claw spawns a subagent only where the host wires a subagent runner into it " +
		"(ClawDefaults.Subagent, built by model.NewSubagentRunner), and the host that built " +
		"this registry wired none: claw's own executor reports a started subagent and launches nothing",
	Remedy: "Wire the runner where the host builds the claw tool set, as iterion's own hosts do in " +
		"process and in the sandbox; or do the work in this node, or route it to claude_code, " +
		"whose native Agent tool (legacy name Task) spawns real subagents",
}

// SetClawRefusedBuiltinForTest adds a refusal for the duration of a test and
// returns the restore function.
//
// It exists for ONE thing no production path can give any more: a list entry.
// The catalog is empty, so the mechanisms that read it — C159 in pkg/dsl/ir,
// the run-time refusal in pkg/backend/tool — would be exercised by nothing,
// and a mechanism no test can fire is a mechanism nobody knows still works.
// Pass a name the catalog HOLDS: a refusal is for a tool the registry serves.
func SetClawRefusedBuiltinForTest(name string, r ClawRefusal) func() {
	previous, had := clawRefusedBuiltins[name]
	clawRefusedBuiltins[name] = r
	return func() {
		if had {
			clawRefusedBuiltins[name] = previous
			return
		}
		delete(clawRefusedBuiltins, name)
	}
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
