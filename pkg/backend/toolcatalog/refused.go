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

// clawRefusedBuiltins is that list: the names claw registers whose executor
// answers success while nothing in iterion consumes what it wrote. Each one
// stays registered — the name resolves, so C135 has nothing to say and a
// declaring workflow still compiles — and the list has two readers: pkg/dsl/ir
// warns at compile time (C159) and pkg/backend/tool refuses every call at run
// time (registerClawTool), with one refusal feeding the advertised description
// and the error.
//
// The three families on it write to registries RegisterClawAll builds fresh
// for each run and nothing reads back: the cron_* and team_* entries live in
// claw's in-memory team.CronRegistry / team.TeamRegistry, which no scheduler
// and no engine logic consults, and `lsp` dispatches against an lsp.Registry
// where no language server is ever registered and no transport is ever set.
// `agent` left the list when the subagent runner was wired at both hosts —
// in process (pkg/runview) and in the container (cmd/iterion) (#1940): a name
// that stays here after its capability becomes real warns about a tool that
// works, which teaches authors to ignore diagnostics.
//
// A deployment whose own host wires no subagent runner is a different fact,
// decided at run time and answered by ClawUnwiredSubagentRefusal: this list is
// about what ITERION cannot serve, which is what a compiler can know.
var clawRefusedBuiltins = map[string]ClawRefusal{
	"cron_create": clawCronRefusal,
	"cron_get":    clawCronRefusal,
	"cron_list":   clawCronRefusal,
	"cron_delete": clawCronRefusal,
	"team_create": clawTeamRefusal,
	"team_get":    clawTeamRefusal,
	"team_list":   clawTeamRefusal,
	"team_delete": clawTeamRefusal,
	"lsp":         clawLSPRefusal,
}

// clawCronRefusal answers the cron_* family.
var clawCronRefusal = ClawRefusal{
	Reason: "claw keeps a cron entry in an in-memory registry that lives as long as this run and that " +
		"nothing reads back: no scheduler ever fires it, and the entry dies with the run that created it",
	Remedy: "Arm a durable schedule through the platform instead (a team schedule under Automations, " +
		"or the schedules API), not through this tool",
}

// clawTeamRefusal answers the team_* family.
var clawTeamRefusal = ClawRefusal{
	Reason: "claw keeps a team in an in-memory registry that nothing in iterion reads back: the grouping " +
		"exists only inside this run's process and drives no task, worker or report",
	Remedy: "Keep the grouping in the node's own output, or in a file of the workspace the next node reads",
}

// clawLSPRefusal answers the `lsp` tool.
var clawLSPRefusal = ClawRefusal{
	Reason: "no language server is ever registered or started for claw in iterion, so diagnostics are " +
		"always empty and every other action has no server to reach",
	Remedy: "Run the language tooling through bash instead: the compiler, linter or type checker of the " +
		"project (go vet, tsc, pyright, cargo check)",
}

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
// It drives the readers of the list — C159 in pkg/dsl/ir, the run-time
// refusal in pkg/backend/tool — over a name of the test's choosing, so their
// reductions, scopes and hedges are exercised on a tool the production list
// does not hold. Pass a name the catalog HOLDS: a refusal is for a tool the
// registry serves.
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
