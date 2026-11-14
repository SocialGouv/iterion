package ir

import (
	"testing"
)

// C341: a secret name whose placeholder spells another's redact-only form is
// refused — the token the engine writes where that other secret must
// resolve nowhere would resolve to this one's value.
func TestC341ASecretNameEndingInRedactedIsRefused(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"pw_redacted": true,
		"redacted":    false,
		"pw":          false,
		"redacted_pw": false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := "dsl: 2\n\nsecrets:\n  " + name + ":\n    optional: true\n\nschema out:\n  ok: bool\n\ntool post:\n  command: `printf '{\"ok\":true}'`\n  output: out\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: post\n  post -> done\n"
			res := compileText(t, src)
			var got []Diagnostic
			for _, d := range res.Diagnostics {
				if d.Code == DiagSecretNameReserved {
					got = append(got, d)
				}
			}
			if want != (len(got) == 1) || len(got) > 1 {
				t.Fatalf("C341 fired %d time(s) for %q, want %v:\n%v", len(got), name, want, res.Diagnostics)
			}
			if want && got[0].Severity != SeverityError {
				t.Errorf("C341 is %q, want an error", got[0].Severity)
			}
		})
	}
}
