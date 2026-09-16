package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func compileText(t *testing.T, src string) *CompileResult {
	t.Helper()
	pr := parser.Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	return Compile(pr.File)
}

const loopRefHead = `schema verdict:
  ok: bool

prompt again:
  Try {{loop.retry.iteration}} once more.

agent check:
  model: "m"
  user: again
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

// A {{loop.<name>.<field>}} reference names a loop an edge declares and a
// field the namespace has (C147): the runtime renders the counters of an
// unknown loop as 0, silently.
func TestALoopReferenceNamesADeclaredLoop(t *testing.T) {
	for name, tc := range map[string]struct {
		ref  string
		want string // "" = no C147
	}{
		"iteration of the declared loop": {"{{loop.retry.iteration}} of {{loop.retry.max}}", ""},
		"previous output, drilled":       {"{{loop.retry.previous_output.ok}}", ""},
		"a loop no edge declares":        {"{{loop.TYPO.iteration}}", `undeclared loop "TYPO"`},
		"a field the namespace has not":  {"{{loop.retry.count}}", `unknown loop field "count"`},
		"a sub-field of the counter":     {"{{loop.retry.iteration.x}}", "has no sub-field"},
	} {
		t.Run(name, func(t *testing.T) {
			cr := compileText(t, strings.Replace(loopRefHead, "%s", tc.ref, 1))
			var got *Diagnostic
			for i := range cr.Diagnostics {
				if cr.Diagnostics[i].Code == DiagUnknownLoopRef {
					got = &cr.Diagnostics[i]
				}
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("C147 on a sound loop reference: %s", got.Message)
				}
				return
			}
			if got == nil || got.Severity != SeverityError || !strings.Contains(got.Message, tc.want) || got.NodeID != "say" {
				t.Fatalf("no C147 error at say saying %q: %+v\n%v", tc.want, got, cr.Diagnostics)
			}
		})
	}
	// The prompt path is the same walk: a typo in a prompt is named at the
	// node that reads it.
	src := strings.Replace(strings.Replace(loopRefHead, "%s", "x", 1), "{{loop.retry.iteration}}", "{{loop.rety.iteration}}", 1)
	cr := compileText(t, src)
	var found bool
	for _, d := range cr.Diagnostics {
		if d.Code == DiagUnknownLoopRef && d.NodeID == "check" && strings.Contains(d.Message, `"rety"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("a loop typo in a prompt was not named: %v", cr.Diagnostics)
	}
}
