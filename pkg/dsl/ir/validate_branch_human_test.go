package ir

import "testing"

func TestValidateTrunkOnlyHumanModesInFanOutBody(t *testing.T) {
	for _, mode := range []string{"review", "llm_or_human"} {
		t.Run(mode, func(t *testing.T) {
			src := `
schema gate_out:
  approved: bool

human gate:
  interaction: ` + mode + `
  model: "test-model"
  output: gate_out

agent other:
  model: "test-model"
  output: gate_out

router dispatch:
  mode: fan_out_all

human collect:
  output: gate_out
  await: wait_all

workflow test:
  entry: dispatch
  worktree: auto
  dispatch -> gate
  dispatch -> other
  gate -> collect
  other -> collect
  collect -> done
`
			result := compileFile(t, src)
			expectDiag(t, result, DiagHumanModeInExecBranch)
		})
	}
}

func TestValidatePlainHumanModeInFanOutBodyAllowed(t *testing.T) {
	src := `
schema gate_out:
  approved: bool

human gate:
  interaction: human
  output: gate_out

agent other:
  model: "test-model"
  output: gate_out

router dispatch:
  mode: fan_out_all

human collect:
  output: gate_out
  await: wait_all

workflow test:
  entry: dispatch
  dispatch -> gate
  dispatch -> other
  gate -> collect
  other -> collect
  collect -> done
`
	result := compileFile(t, src)
	expectNoDiag(t, result, DiagHumanModeInExecBranch)
}

func TestValidateTrunkOnlyModesOnDownstreamBranchLoopHead(t *testing.T) {
	tests := []struct {
		name     string
		decl     string
		diagCode DiagCode
	}{
		{
			name: "persist",
			decl: `agent refine:
  model: "test-model"
  output: s
  session: persist`,
			diagCode: DiagPersistInFanOut,
		},
		{
			name: "review",
			decl: `human refine:
  interaction: review
  model: "test-model"
  output: s`,
			diagCode: DiagHumanModeInExecBranch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := execBranchLoopPrompts + `
tool gen:
  command: ` + "`echo`" + `
  output: s

tool writer:
  command: ` + "`echo`" + `
  output: s

tool collect:
  command: ` + "`echo`" + `
  output: s

` + tc.decl + `

router items:
  mode: fan_out_each
  over: "{{outputs.gen.items}}"
  as: item

workflow test:
  entry: gen
  worktree: auto
  gen -> items
  items -> writer
  writer -> collect
  collect -> refine
  refine -> refine as more(3) when ok
  refine -> done else
`
			result := compileFile(t, src)
			expectDiag(t, result, tc.diagCode)
		})
	}
}
