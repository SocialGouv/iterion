package cli_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A compute guard whose bool field is fed a string, and whose int field is fed
// a fraction: #792 made both fail the node on every run.
const computeGuardBot = `dsl: 2

schema probe_out:
  verdict: json

schema gauge:
  used_pct: int
  exhausted: bool

tool probe:
  command: "printf '{\"verdict\":\"true\"}'"
  output: probe_out

compute gate:
  output: gauge
  expr:
    used_pct: "10.58"
    exhausted: "outputs.probe.verdict"

fail stop:
  description: "the guard tripped"
  code: GUARD_TRIPPED
  message: "guard tripped at {{outputs.gate.used_pct}}"
  resumable: true

workflow guard:
  worktree: none
  sandbox: none
  entry: probe
  probe -> gate
  gate -> stop when exhausted
  gate -> done else
`

func TestR3ComputeTypingOnTheRealRunPath(t *testing.T) {
	for _, strict := range []string{"", "true"} {
		t.Run("strict="+strict, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("ITERION_STRICT_OUTPUT_VALIDATION", strict)
			path := r3WriteBot(t, computeGuardBot)
			r := r3RunBot(t, path, "guard")
			if r.err == nil || r.run.FailureCode != store.FailureSchemaValidation || r.run.Status != "failed_resumable" {
				t.Fatalf("#792 guard escaped: status=%s code=%s err=%v", r.run.Status, r.run.FailureCode, r.err)
			}
			var gate map[string]any
			if r.run.Checkpoint != nil {
				gate = r.run.Checkpoint.Outputs["gate"]
			}
			var finished []string
			mismatch := 0
			for _, ev := range r.events {
				if ev.Type == store.EventNodeFinished {
					finished = append(finished, ev.NodeID)
				}
				if ev.Type == store.EventNodeOutputSchemaMismatch {
					mismatch++
					t.Logf("mismatch event node=%s data=%v", ev.NodeID, ev.Data)
				}
			}
			t.Logf("strict=%q status=%s code=%s err=%v", strict, r.run.Status, r.run.FailureCode, r.err)
			t.Logf("gate output: used_pct=%v (%T) exhausted=%v (%T)", gate["used_pct"], gate["used_pct"], gate["exhausted"], gate["exhausted"])
			t.Logf("finished nodes=%v mismatch events=%d", finished, mismatch)
		})
	}
}
