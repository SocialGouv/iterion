package ir

import "testing"

// A node's `with { ... }` values are templates, and until they were walked
// they were the one template family no pass read. The tests below drive each
// kind that carries one, and each fixture differs from its neighbour by the
// single thing it is about.

const subbotWithUndeclaredVar = `
vars:
  goal: string

schema vout:
  validated: bool

subbot child:
  source: "kid.bot"
  with { goal: "{{vars.goal}}", depth: "{{vars.depth}}" }
  output: vout

workflow test:
  entry: child
  child -> done
`

const subbotWithDeclaredVarsOnly = `
vars:
  goal: string
  depth: string

schema vout:
  validated: bool

subbot child:
  source: "kid.bot"
  with { goal: "{{vars.goal}}", depth: "{{vars.depth}}" }
  output: vout

workflow test:
  entry: child
  child -> done
`

const emitWithUndeclaredVar = `
schema s:
  ok: bool

tool t:
  command: ` + "`echo hi`" + `
  output: s

emit ping:
  event: "ping"
  with { who: "{{vars.nobody}}" }

wait pong:
  event: "ping"
  timeout: "1m"

workflow test:
  entry: t
  t -> ping
  ping -> done
`

// A `fan_out_each` router declares no `output:` — it cannot — yet the keys it
// passes through are known: the element under its `as:` name, plus `item`,
// `index`, `count`. This is the canonical fan-out-to-subbot shape, shipped as
// a scaffold template.
const subbotReadingAFanOutElement = `
schema list_out:
  items: json

schema vout:
  validated: bool

tool list:
  command: ` + "`echo hi`" + `
  output: list_out

router dispatch:
  mode: fan_out_each
  over: "{{outputs.list.items}}"
  as: ticket

subbot handle:
  source: "kid.bot"
  with { ticket_id: "{{outputs.dispatch.ticket.id}}" }
  output: vout

workflow test:
  entry: list
  list -> dispatch
  dispatch -> handle
  handle -> done
`

// The same shape with a key the router does not bind.
const subbotReadingAKeyTheRouterNeverBinds = `
schema list_out:
  items: json

schema vout:
  validated: bool

tool list:
  command: ` + "`echo hi`" + `
  output: list_out

router dispatch:
  mode: fan_out_each
  over: "{{outputs.list.items}}"
  as: ticket

subbot handle:
  source: "kid.bot"
  with { ticket_id: "{{outputs.dispatch.tickets.id}}" }
  output: vout

workflow test:
  entry: list
  list -> dispatch
  dispatch -> handle
  handle -> done
`

// TestASubbotReadingAFanOutElementIsNotSecondGuessed.
//
// Warning here would hand the author a remedy they cannot follow: "add an
// output schema" to a node kind that takes none. A warning nobody can act on
// is how a canonical shape teaches operators to ignore diagnostics.
func TestASubbotReadingAFanOutElementIsNotSecondGuessed(t *testing.T) {
	r := compileFile(t, subbotReadingAFanOutElement)
	expectNoDiag(t, r, DiagRefNodeNoSchema)
}

// TestAKeyTheRouterNeverBindsIsStillFlagged is the other half: the carve-out
// above must let through the typo it exists to keep quiet about, and nothing
// else. `tickets` is not the binding, not `item`, not `index`, not `count`,
// and no incoming edge carries it.
func TestAKeyTheRouterNeverBindsIsStillFlagged(t *testing.T) {
	r := compileFile(t, subbotReadingAKeyTheRouterNeverBinds)
	expectDiag(t, r, DiagRefNodeNoSchema)
}

// TestASubbotHandsItsChildOnlyVarsThisProgramDeclares.
//
// The value is not merely unresolved: it is handed over, empty. An undeclared
// var resolves to nil and is spliced away, so the child run starts with an
// empty depth — quieter than a literal would have been — and nothing
// downstream re-reads it. An unvalidated mapping is a wrong argument
// delivered in silence rather than a diagnostic.
func TestASubbotHandsItsChildOnlyVarsThisProgramDeclares(t *testing.T) {
	r := compileFile(t, subbotWithUndeclaredVar)
	expectDiag(t, r, DiagUndeclaredVar)
}

// TestASubbotWithOnlyDeclaredVarsIsAccepted pins the end state the check
// drives toward — without it, a check that refused every `with:` would look
// exactly as green as one that refuses the right ones.
func TestASubbotWithOnlyDeclaredVarsIsAccepted(t *testing.T) {
	r := compileFile(t, subbotWithDeclaredVarsOnly)
	expectNoDiag(t, r, DiagUndeclaredVar)
}

// TestAnEmitPublishesOnlyVarsThisProgramDeclares is the second kind carrying a
// `with:`. It is here because the defect was reported on subbot alone, and a
// fix that closed only the reported site would have left the class open.
func TestAnEmitPublishesOnlyVarsThisProgramDeclares(t *testing.T) {
	r := compileFile(t, emitWithUndeclaredVar)
	expectDiag(t, r, DiagUndeclaredVar)
}
