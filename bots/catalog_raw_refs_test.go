package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// rawRef matches the bang form `{{!vars.x}}` / `{{!input.x}}` / `{{!outputs…}}`,
// which the runtime substitutes VERBATIM into a shell command instead of
// shell-escaping it (ir.Ref.Unquoted; see
// model.TestRawRefsBypassEscapingByDesign).
var rawRef = regexp.MustCompile(`\{\{\s*!\s*[A-Za-z0-9_.]+\s*\}\}`)

// Every ordinary `{{vars.x}}` in a tool command is shell-escaped, which is
// what makes the catalog's bare `VAR={{vars.x}} python3 -c "…"` shape safe
// with forge-controlled values (a PR body, a branch name, an issue title).
// The bang form opts OUT of that, one ref at a time, silently: the diff of
// adding a `!` is one byte, and the value it lets through lands in `sh -c`
// as syntax.
//
// No catalog bot needs it today — this test is the fence around that fact,
// placed while the count is zero and the argument is cheap. A bot that
// genuinely must re-interpret an upstream shell snippet adds itself to the
// allowlist below WITH the reason, which is exactly the review conversation
// that should happen before a forge-controlled value reaches a shell
// unescaped.
func TestCatalogToolCommandsDoNotOptOutOfShellEscaping(t *testing.T) {
	// Bot id → why its raw ref is safe. Empty today, deliberately.
	allowed := map[string]string{}

	roots, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) == 0 {
		t.Fatal("no catalog bots found — the lint would pass vacuously")
	}
	for _, path := range roots {
		bot := filepath.Dir(path)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, cmd := range commandBackticks(string(src)) {
			for _, hit := range rawRef.FindAllString(cmd, -1) {
				if why, ok := allowed[bot]; ok {
					t.Logf("%s: raw ref %s allowed (%s)", bot, hit, why)
					continue
				}
				t.Errorf("%s: tool command uses the raw ref %s, which bypasses shell escaping.\n"+
					"A forge-controlled value (PR body, branch name, issue title) then reaches `sh -c` as SYNTAX.\n"+
					"Use the ordinary %s form, or add %q to this test's allowlist with the reason.",
					bot, hit, strings.Replace(hit, "!", "", 1), bot)
			}
		}
	}
}

// C137 is a WARNING (a repo full of the shape still compiles while it is
// cleaned up), so nothing stops it drifting back into the catalog. Here it is
// an error: the catalog is the reference implementation, and the shape is a
// command-execution hazard on any forge-controlled value.
func TestCatalogHasNoAuthorQuotedRefs(t *testing.T) {
	paths, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no catalog bots found — the lint would pass vacuously")
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		pr := parser.Parse(path, string(src))
		if pr.File == nil {
			t.Logf("%s: not inspected (unparseable — the parse/compile test owns that)", path)
			continue
		}
		cr := ir.Compile(pr.File)
		// C137 is emitted by the compiler; the parser's own diagnostics are a
		// different type and a different concern.
		for _, d := range cr.Diagnostics {
			if d.Code == ir.DiagQuotedCommandRef {
				t.Errorf("%s: %s\n(the runtime shell-quotes refs; author quotes CANCEL that and the value becomes shell syntax)",
					path, d.Message)
			}
		}
	}
}
