package ir

import (
	"strings"
	"testing"
)

const quotedRefHead = `vars:
  base: string = "main"

schema verdict:
  ok: bool

agent check:
  model: "m"
  output: verdict

tool say:
  command: "echo %s"

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

// A quoted reference in a tool body is a warning (C137) — and an error
// when it reads an artifact, an attachment or a loop counter: those
// namespaces resolve in a tool body since 3.151, and a bot carrying the
// shape would go from an inert command to an armed one at upgrade. A raw
// reference (`{{!…}}`) is a warning in its own words whatever the
// namespace: the runtime does not escape it, the quotes are its only
// containment.
func TestAQuotedRefThatReachesTheShellFromAnotherNodeIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		ref      string
		severity Severity
	}{
		"a var, quoted":                        {"'{{vars.base}}'", SeverityWarning},
		"a loop counter, quoted":               {"'{{loop.retry.iteration}}'", SeverityError},
		"a loop counter, raw, bang and spaces": {"'{{ ! loop.retry.max }}'", SeverityWarning},
		"a var, raw":                           {"'{{!vars.base}}'", SeverityWarning},
	} {
		t.Run(name, func(t *testing.T) {
			cr := compileText(t, strings.Replace(quotedRefHead, "%s", tc.ref, 1))
			var got *Diagnostic
			for i := range cr.Diagnostics {
				if cr.Diagnostics[i].Code == DiagQuotedCommandRef {
					got = &cr.Diagnostics[i]
				}
			}
			if got == nil || got.Severity != tc.severity || got.NodeID != "say" {
				t.Fatalf("C137 for %s: %+v\n%v", tc.ref, got, cr.Diagnostics)
			}
			if tc.severity == SeverityError && !strings.Contains(got.Message, "command execution") {
				t.Fatalf("the error does not say what is at stake: %s", got.Message)
			}
			// A raw reference is warned of in its own words — the quotes are
			// its only containment — never as the cancelling of two quotings.
			if strings.Contains(tc.ref, "!") && !strings.Contains(got.Message, "only containment") {
				t.Fatalf("a raw reference read as a double quoting: %s", got.Message)
			}
		})
	}
}
