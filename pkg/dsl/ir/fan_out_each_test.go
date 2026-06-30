package ir

import "testing"

// A well-formed fan_out_each (with `over:` and exactly one outgoing template
// edge) raises none of the fan_out_each diagnostics.
func TestValidateFanOutEach_Valid(t *testing.T) {
	src := `schema s:
  ok: bool

tool gen:
  command: ` + "`echo`" + `
  output: s

tool h:
  command: ` + "`echo`" + `
  input: s
  output: s

tool collect:
  command: ` + "`echo`" + `
  output: s

router dispatch:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"
  as: task

workflow w:
  entry: gen
  gen -> dispatch
  dispatch -> h
  h -> collect
  collect -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagFanOutEachMissingOver)
	expectNoDiag(t, r, DiagFanOutEachOnlyProperty)
	expectNoDiag(t, r, DiagFanOutEachEdges)
}

// C113 — fan_out_each without an `over:` source.
func TestValidateFanOutEach_MissingOver(t *testing.T) {
	src := `tool gen:
  command: ` + "`echo`" + `

tool h:
  command: ` + "`echo`" + `

router dispatch:
  mode: fan_out_each

workflow w:
  entry: gen
  gen -> dispatch
  dispatch -> h
  h -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagFanOutEachMissingOver)
}

// C113 — depends_on without key is ambiguous (no id to resolve deps against).
func TestValidateFanOutEach_DependsOnWithoutKey(t *testing.T) {
	src := `tool gen:
  command: ` + "`echo`" + `

tool h:
  command: ` + "`echo`" + `

router dispatch:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"
  depends_on: deps

workflow w:
  entry: gen
  gen -> dispatch
  dispatch -> h
  h -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagFanOutEachMissingOver)
}

// C114 — `over:` on a non-fan_out_each router.
func TestValidateFanOutEach_OverOnNonFanOutEach(t *testing.T) {
	src := `schema s:
  ok: bool

tool gen:
  command: ` + "`echo`" + `
  output: s

tool h:
  command: ` + "`echo`" + `

tool other:
  command: ` + "`echo`" + `

router dispatch:
  mode: condition
  over: "{{outputs.gen.items}}"

workflow w:
  entry: gen
  gen -> dispatch
  dispatch -> h when ok
  dispatch -> other
  h -> done
  other -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagFanOutEachOnlyProperty)
}

// C115 — fan_out_each must have exactly one outgoing template edge.
func TestValidateFanOutEach_TooManyEdges(t *testing.T) {
	src := `tool gen:
  command: ` + "`echo`" + `

tool h1:
  command: ` + "`echo`" + `

tool h2:
  command: ` + "`echo`" + `

router dispatch:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"

workflow w:
  entry: gen
  gen -> dispatch
  dispatch -> h1
  dispatch -> h2
  h1 -> done
  h2 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagFanOutEachEdges)
}
