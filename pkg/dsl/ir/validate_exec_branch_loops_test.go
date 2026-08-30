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

func TestValidateLoopInFanOutAllBody_Allowed(t *testing.T) {
	// The bounded edge and its target are owned by the a1 branch; join remains
	// the structural collector shared with a2.
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
	expectNoDiag(t, r, DiagLoopInExecBranch)
	expectNoDiag(t, r, DiagImplicitCollectorMove)
}

func TestValidateLoopHeadInFanOutBody_Allowed(t *testing.T) {
	// A bounded self-edge is local control flow, not another structural
	// predecessor that could elect the branch head as the collector.
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
	expectNoDiag(t, r, DiagLoopInExecBranch)
	expectNoDiag(t, r, DiagImplicitCollectorMove)
}

func TestValidateLoopImplReviewRetryInFanOut_Allowed(t *testing.T) {
	// The idiomatic per-item retry: impl → review → impl as fix. review is
	// the loop source and is not a structural join (one non-iteration
	// predecessor), so the whole cycle has one branch owner.
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
	expectNoDiag(t, r, DiagLoopInExecBranch)
	expectNoDiag(t, r, DiagImplicitCollectorMove)
}

func TestValidateLoopInFanOutEachBody_Allowed(t *testing.T) {
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
	expectNoDiag(t, r, DiagLoopInExecBranch)
	expectNoDiag(t, r, DiagImplicitCollectorMove)
}

func TestValidateForeachInFanOutBody_Allowed(t *testing.T) {
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
	expectNoDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopInLLMMultiBody_Allowed(t *testing.T) {
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
	expectNoDiag(t, r, DiagLoopInExecBranch)
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

func TestValidateLoopReenteringBodyFromJoin_Rejected(t *testing.T) {
	// join -> a1 as more is not a wrap-from-join. findConvergencePoint
	// counts the back-edge, elects a1, the a1 branch runs nothing, and
	// the a2 branch swallows wait_all. C244 keys on the edge target too.
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
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopInFanOutEachImplicitChain_WarnsMigration(t *testing.T) {
	// fan_out_each has one template edge, so writer/collect/refine share one
	// item owner. The refine self-loop runs inside that item scope.
	src := execBranchLoopPrompts + `
tool gen:
  command: ` + "`echo`" + `
  output: s

tool writer:
  command: ` + "`echo`" + `
  input: s
  output: s

tool collect:
  command: ` + "`echo`" + `
  output: s

tool refine:
  command: ` + "`echo`" + `
  output: s

router items:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"
  as: item

workflow test:
  entry: gen
  gen -> items
  items -> writer
  writer -> collect
  collect -> refine
  refine -> refine as more(3) when ok
  refine -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
	expectDiag(t, r, DiagImplicitCollectorMove)
	for _, diag := range r.Diagnostics {
		if diag.Code == DiagImplicitCollectorMove && diag.Severity != SeverityWarning {
			t.Fatalf("C246 severity = %s, want warning", diag.Severity)
		}
	}
}

func TestValidateLoopOnFanOutEachTemplateHead_Allowed(t *testing.T) {
	// writer is the only fan target. Its bounded self-loop stays inside each
	// item scope and does not elect the template head as an implicit collector.
	src := execBranchLoopPrompts + `
tool gen:
  command: ` + "`echo`" + `
  output: s

tool writer:
  command: ` + "`echo`" + `
  input: s
  output: s

router items:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"
  as: item

workflow test:
  entry: gen
  gen -> items
  items -> writer
  writer -> writer as retry(3) when ok
  writer -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
	expectNoDiag(t, r, DiagImplicitCollectorMove)
}

func TestValidateLoopNestedFanOutEachInFanOutAll_Rejected(t *testing.T) {
	// fan_out_each nested in a fan_out_all body. The inner walk stops at
	// refine (single-path election); the outer walk does not. Sharing one
	// visited set made C244 depend on w.Nodes map order (R2db4d4).
	src := execBranchLoopPrompts + `
tool a1:
  command: ` + "`echo`" + `
  output: s

tool a2:
  command: ` + "`echo`" + `
  output: s

tool writer:
  command: ` + "`echo`" + `
  input: s
  output: s

tool refine:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

router items:
  mode: fan_out_each
  over: "{{outputs.a1.items}}"
  as: item

tool join:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> items
  items -> writer
  writer -> refine
  refine -> refine as more(3) when ok
  refine -> join else
  a2 -> join
  join -> done
`
	for i := 0; i < 50; i++ {
		r := compileFile(t, src)
		expectDiag(t, r, DiagLoopInExecBranch)
	}
}

func TestValidateLoopAfterNonElectedAwait_Allowed(t *testing.T) {
	// joiner is the elected convergence. A second await node on the a2 path
	// does not stop execBranch, so w -> z remains local to that branch and is
	// safe to execute with its private counters.
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

func TestValidateLoopNameSharedByTrunkAndBranchAfterNonElectedAwait_Rejected(t *testing.T) {
	// joiner is elected from the first branch, so z is only an await-looking
	// node on the second branch: execBranch runs through it. The compiler must
	// therefore classify w -> z as branch-local and reject its name colliding
	// with the trunk loop below joiner.
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

tool joiner:
  command: ` + "`echo`" + `
  output: s
  await: wait_all

tool trunk:
  command: ` + "`echo`" + `
  output: s

router r1:
  mode: fan_out_all

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
  joiner -> trunk
  trunk -> joiner as refine(3) when ok
  trunk -> done else
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
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

func TestValidateLoopNameSharedByTrunkAndBranch_Rejected(t *testing.T) {
	// Each edge passes the per-edge ownership test (a1 -> dummy is owned by
	// the a1 branch, join -> r1 is on the trunk), but both back-edges share
	// the loop name, so the compiler folds them into one Loop and the branch's
	// local counter would shadow the enclosing trunk counter at run time.
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
  a1 -> dummy as retry(3) when ok
  a1 -> join else
  dummy -> join
  a2 -> join
  join -> r1 as retry(3) when ok
  join -> done else
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateLoopNameSharedBySiblingBranches_Rejected(t *testing.T) {
	// Two sibling branches each own a self-loop under the same name: neither
	// edge crosses a boundary, but the name would map onto one durable Loop
	// whose counters the siblings cannot share.
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
  a1 -> a1 as retry(3) when ok
  a1 -> join else
  a2 -> a2 as retry(3) when ok
  a2 -> join else
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLoopInExecBranch)
}

func TestValidateDistinctLoopNamesAcrossScopes_Allowed(t *testing.T) {
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
  a1 -> a1 as inner(3) when ok
  a1 -> join else
  a2 -> join
  join -> r1 as outer(3) when ok
  join -> done else
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLoopInExecBranch)
}
