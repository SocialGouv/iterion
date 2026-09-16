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

// TestASubbotHandsItsChildOnlyVarsThisProgramDeclares.
//
// The value is not merely unresolved: it is handed over. A child run starts
// with the literal `{{vars.depth}}` as its depth, and nothing downstream
// re-reads it, so an unvalidated mapping is a wrong argument delivered in
// silence rather than a diagnostic.
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
