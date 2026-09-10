package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// `worktree:` takes auto or none; any other word is refused at compile
// (C142), named as WRITTEN — the runtime compares the canonical value to
// `auto` and nothing else, so a typo used to run the workflow in place, in
// the operator's own checkout, without a word. The canonical form is what
// the IR carries: `AUTO` must reach the runtime as `auto`, or the accepted
// spelling would be refused by the runtime's exact comparison.
func TestWorktreeValueIsCheckedAtCompile(t *testing.T) {
	compile := func(worktree string) (string, []string) {
		src := "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n" + worktree + "  a -> done\n"
		pr := parser.Parse("wt.bot", src)
		if len(pr.Diagnostics) != 0 {
			t.Fatalf("parse: %v", pr.Diagnostics)
		}
		res := Compile(pr.File)
		var errs []string
		for _, d := range res.Diagnostics {
			if d.Severity == SeverityError {
				errs = append(errs, string(d.Code)+": "+d.Message)
			}
		}
		mode := ""
		if res.Workflow != nil {
			mode = res.Workflow.Worktree
		}
		return mode, errs
	}
	for src, want := range map[string]string{"": "auto", "  worktree: auto\n": "auto", "  worktree: none\n": "none", "  worktree: AUTO\n": "auto"} {
		mode, errs := compile(src)
		if len(errs) != 0 || mode != want {
			t.Errorf("%q: mode %q, errors %v (want %q, none)", src, mode, errs, want)
		}
	}
	_, errs := compile("  worktree: ATUO\n")
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "C142: ") || !strings.Contains(errs[0], `invalid worktree "ATUO"`) {
		t.Fatalf("want one C142 naming the value as written, got %v", errs)
	}
}
