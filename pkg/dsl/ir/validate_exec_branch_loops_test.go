package ir

import "testing"

// C243 — bounded iteration (loop / foreach) inside a subgraph that
// execBranch runs (fan_out_all, fan_out_each, llm multi).

const execBranchLoopPrompts = `
prompt sys:
  System.

prompt usr:
  User.

schema s:
  ok: bool
  items: json
`

func TestValidateLoopInFanOutAllBody_Rejected(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> a1 as refine(5) when ok
  a1 -> join else
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopInFanOutEachBody_Rejected(t *testing.T) {
	src := execBranchLoopPrompts + `
tool gen:
  command: ` + "`echo`" + `
  output: s

tool writer:
  command: ` + "`echo`" + `
  input: s
  output: s

tool verdict:
  command: ` + "`echo`" + `
  input: s
  output: s

router items:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"
  as: item

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: gen
  gen -> items
  items -> writer
  writer -> verdict
  verdict -> writer as refine(5) when not ok
  verdict -> join else
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateForeachInFanOutBody_Rejected(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> a1 as foreach scan(item in "{{outputs.a1.items}}")
  a1 -> join
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopInLLMMultiBody_Rejected(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: llm
  model: "test-model"
  multi: true

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> a1 as refine(5) when ok
  a1 -> join else
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopOnLLMSingle_Allowed(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: llm
  model: "test-model"

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> a1 as refine(5) when ok
  a1 -> done else
  a2 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopWrappingFanOut_Allowed(t *testing.T) {
	src := execBranchLoopPrompts + `
tool gen:
  command: ` + "`echo`" + `
  output: s

tool h:
  command: ` + "`echo`" + `
  input: s
  output: s

router dispatch:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"
  as: task

tool collect:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: gen
  gen -> dispatch
  dispatch -> h
  h -> collect
  collect -> dispatch as outer(3) when ok
  collect -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopAfterJoin_Allowed(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> join
  a2 -> join
  join -> join as more(3) when ok
  join -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopAfterImplicitJoin_Allowed(t *testing.T) {
	// collect has two incoming edges and no await: — findConvergencePoint
	// still treats it as the join, so a loop on collect runs on the trunk.
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

tool collect:
  command: ` + "`echo`" + `
  output: s

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> collect
  a2 -> collect
  collect -> collect as more(3) when ok
  collect -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopOnFanOutRouterEdge_Rejected(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1 as more(3)
  r1 -> a2
  a1 -> join
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopReenteringBodyFromJoin_Allowed(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> join
  a2 -> join
  join -> a1 as more(3) when ok
  join -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopOnTrunk_Allowed(t *testing.T) {
	src := execBranchLoopPrompts + `
tool a:
  command: ` + "`echo`" + `
  output: s

workflow test:
  entry: a
  a -> a as refine(3) when ok
  a -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
}
