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
