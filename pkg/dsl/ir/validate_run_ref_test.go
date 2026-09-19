package ir

import (
	"strings"
	"testing"
)

const runRefHead = `schema verdict:
  ok: bool

prompt again:
  Spent {{run.cost_usd}}; noise: {{run.tree_noise}}

agent check:
  model: "m"
  user: again
  output: verdict

tool say:
  command: "git add -A -- ':/' $ITERION_TREE_NOISE"

workflow w:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 20
  check -> say when not ok as retry(2)
  check -> done when ok
  say -> check
`

// A {{run.<member>}} reference whose member the namespace does not carry is
// C149 (a warning, like C147 for loops): the runtime renders no value for it
// (resolveRunPath returns nil), and "renders empty" is exactly how an
// exclusion list vanishes from a scope gate's git command (#1464). The
// compiler naming it at validate time is the guard; the plancher
// (requires.iterion) stays the guard for engines older than the diagnostic.
func TestARunReferenceNamesAKnownMember(t *testing.T) {
	for name, tc := range map[string]struct {
		ref  string
		want string // "" = no C149
	}{
		"the budget members":      {"{{run.cost_usd}} and {{run.max_cost_usd}}", ""},
		"the noise member":        {"{{run.tree_noise}}", ""},
		"a member nobody carries": {"{{run.tree_nose}}", `unknown run member "tree_nose"`},
		"a sub-field of a scalar": {"{{run.tree_noise.list}}", "has no sub-field"},
		"the id, drilled":         {"{{run.id.x}}", "has no sub-field"},
	} {
		t.Run(name, func(t *testing.T) {
			cr := compileText(t, strings.Replace(runRefHead, "{{run.cost_usd}}", tc.ref, 1))
			var got *Diagnostic
			for i := range cr.Diagnostics {
				if cr.Diagnostics[i].Code == DiagUnknownRunMember {
					got = &cr.Diagnostics[i]
				}
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("C149 on a sound run reference: %s", got.Message)
				}
				return
			}
			if got == nil || got.Severity != SeverityWarning || !strings.Contains(got.Message, tc.want) {
				t.Fatalf("no C149 warning saying %q: %+v\n%v", tc.want, got, cr.Diagnostics)
			}
		})
	}

	// The typo is named at the node that reads it — here the prompt of
	// `check`; the command of `say` carries the sound member and must stay
	// quiet. (The loop twin of this test makes the same walk for prompts.)
	cr := compileText(t, strings.Replace(runRefHead, "{{run.cost_usd}}", "{{run.noise_lust}}", 1))
	var found bool
	for _, d := range cr.Diagnostics {
		if d.Code == DiagUnknownRunMember && d.NodeID == "check" && strings.Contains(d.Message, `"noise_lust"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("a run-member typo in a prompt was not named: %v", cr.Diagnostics)
	}
}
