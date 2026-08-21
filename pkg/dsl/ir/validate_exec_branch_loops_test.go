package ir

import "testing"

// C244 — bounded iteration (loop / foreach) inside a subgraph that
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
	// Loop target is a distinct dummy so findConvergencePoint still elects
	// join (a1 has a single incoming source). execBranch runs a1 and would
	// skip the loop — that is the C244 true positive.
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

tool dummy:
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
  a1 -> dummy as refine(5) when ok
  a1 -> join else
  dummy -> join
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopHeadElectedAsJoin_Rejected(t *testing.T) {
	// a1 -> a1 as refine gives a1 two incoming sources, so findConvergencePoint
	// elects a1 as the join and "hoists" the loop onto the trunk. That is not
	// a legal wrap: the a1 branch starts on the convergence node (runs
	// nothing), the a2 branch swallows the wait_all join, then the trunk
	// runs the loop and the join a second time. C244 must refuse it.
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
  a1 -> a1 as refine(3) when ok
  a1 -> join else
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopImplReviewRetryInFanOut_Rejected(t *testing.T) {
	// The idiomatic per-item retry: impl → review → impl as fix. review is
	// the loop source and is not a structural join (one non-iteration
	// predecessor). Same mis-execution as the self-loop head.
	src := execBranchLoopPrompts + `
tool impl:
  command: ` + "`echo`" + `
  output: s

tool review:
  command: ` + "`echo`" + `
  output: s

tool other:
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
  r1 -> impl
  r1 -> other
  impl -> review
  review -> impl as fix(3) when not ok
  review -> join else
  other -> join
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

tool dummy:
  command: ` + "`echo`" + `
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
  verdict -> dummy as refine(5) when not ok
  verdict -> join else
  dummy -> join
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

tool dummy:
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
  a1 -> dummy as foreach scan(item in "{{outputs.a1.items}}")
  a1 -> join
  dummy -> join
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

tool dummy:
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
  a1 -> dummy as refine(5) when ok
  a1 -> join else
  dummy -> join
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

func TestValidateLoopAfterImplicitJoin_Allowed(t *testing.T) {
	// collect has two incoming sources and no await:. Runtime treats that
	// as a convergence point (findConvergencePoint); C244 must stop the
	// body walk there too, or a trunk loop after collect is a false positive.
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

tool refine:
  command: ` + "`echo`" + `
  output: s

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> collect
  a2 -> collect
  collect -> refine
  refine -> refine as more(5) when ok
  refine -> done else
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

func TestValidateLoopOnImplicitJoin_Allowed(t *testing.T) {
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

func TestValidateLoopWrappingFanOutFromImplicitJoin_Allowed(t *testing.T) {
	// The documented escape hatch (wrap the router from the join) must
	// still compile when the join omits await: — same implicit join the
	// runtime elects.
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
  collect -> r1 as outer(2) when ok
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
	// join has await, so the structural stop keeps it out of the body.
	// C244 keys on the edge source (join), which is not in the body.
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

func TestValidateLoopAfterNonElectedAwait_NotClaimed(t *testing.T) {
	// joiner is the elected convergence (first fan edge reaches it). z also
	// has await: so the structural stop treats it as a join and does not
	// put w in the body. The skip of w→z inside execBranch is a runtime
	// hole (execBranch only stops at the elected id), not C244.
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

tool x:
  command: ` + "`echo`" + `
  output: s

tool z:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

tool w:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

tool joiner:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> x
  x -> joiner
  a2 -> z
  z -> w
  w -> z as refine(3) when ok
  w -> joiner else
  joiner -> done
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
