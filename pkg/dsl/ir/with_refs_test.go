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

// A branch head reading all four element keys, and a node PAST the join
// reading two of them. The runtime binds the element into the branch's own
// outputs; after the join those are gone, and the reference renders as its
// own source text.
const branchHeadAndPostJoinReaders = `
schema list_out:
  items: json

schema r_out:
  ok: bool

tool list:
  command: ` + "`echo hi`" + `
  output: list_out

router dispatch:
  mode: fan_out_each
  over: "{{outputs.list.items}}"
  as: ticket

tool head:
  command: ` + "`echo {{outputs.dispatch.ticket}} {{outputs.dispatch.item}} {{outputs.dispatch.index}} {{outputs.dispatch.count}}`" + `
  output: r_out

compute collect:
  await: wait_all
  expr:
    done: "true"

workflow test:
  entry: list
  list -> dispatch
  dispatch -> head
  head -> collect
  collect -> done
`

const postJoinReaderOfAnElementKey = `
schema list_out:
  items: json

schema r_out:
  ok: bool

tool list:
  command: ` + "`echo hi`" + `
  output: list_out

router dispatch:
  mode: fan_out_each
  over: "{{outputs.list.items}}"
  as: ticket

tool head:
  command: ` + "`echo hi`" + `
  output: r_out

compute collect:
  await: wait_all
  expr:
    done: "true"

tool report:
  command: ` + "`echo {{outputs.dispatch.index}}`" + `
  output: r_out

workflow test:
  entry: list
  list -> dispatch
  dispatch -> head
  head -> collect
  collect -> report
  report -> done
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

// TestABranchHeadReadsTheElementAndItsPosition covers all four keys the
// silence claims, not just the one the scaffold template happens to use — a
// claim about four keys proven on one is a claim about one.
func TestABranchHeadReadsTheElementAndItsPosition(t *testing.T) {
	r := compileFile(t, branchHeadAndPostJoinReaders)
	expectNoDiag(t, r, DiagRefNodeNoSchema)
}

// TestPastTheJoinTheElementIsGoneAndItIsSaid is the boundary. The silence is
// the branch's, not the router's: measured on the engine, a node after the
// join receives the raw template text for the very same key. Silencing there
// would re-open, inside the fix, the defect this branch exists to close.
func TestPastTheJoinTheElementIsGoneAndItIsSaid(t *testing.T) {
	r := compileFile(t, postJoinReaderOfAnElementKey)
	expectDiag(t, r, DiagRefNodeNoSchema)
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
// The mapping is worse than the key left unmapped. A bare undeclared ref
// resolves to nil and is handed over as nil, which suppresses the child's own
// declared default for `depth`; omitting the key entirely would have let that
// default stand. Nothing downstream re-reads it, so an unvalidated mapping is
// a wrong argument delivered in silence rather than a diagnostic.
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

// A node carrying several keys, two of them wrong, each on its own line.
const subbotWithSeveralKeysOnSeveralLines = `
vars:
  goal: string

schema vout:
  validated: bool

subbot child:
  source: "kid.bot"
  with {
    a: "{{vars.nope1}}",
    b: "{{vars.goal}}",
    c: "{{vars.nope2}}"
  }
  output: vout

workflow test:
  entry: child
  child -> done
`

// TestEachBadMappingIsReportedOnItsOwnLine.
//
// #1281 asks for the check "at the mapping's line", and the edge walk already
// carries a span for exactly this reason. Without one, every key of a
// multi-key `with { … }` reports at the node's header — the author is told
// there is a problem in the block and left to find which key, in the CLI, in
// the studio and in any inline forge rendering.
func TestEachBadMappingIsReportedOnItsOwnLine(t *testing.T) {
	r := compileFile(t, subbotWithSeveralKeysOnSeveralLines)

	lines := map[int]bool{}
	for _, d := range r.Diagnostics {
		if d.Code == DiagUndeclaredVar {
			lines[d.Line] = true
		}
	}
	if len(lines) != 2 {
		t.Fatalf("two bad keys on two lines reported at %d distinct position(s): %v", len(lines), r.Diagnostics)
	}
	// The node header is line 9 of this source; the mappings are 12 and 14.
	if lines[9] {
		t.Errorf("a mapping diagnostic landed on the node's header line: %v", r.Diagnostics)
	}
}
