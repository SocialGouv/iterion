package bots

import (
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestCatalogBotsParseAndCompileClean is the CI floor that every shipped
// catalog workflow (bundles under bots/ + loose .bot files under examples/)
// must PARSE and COMPILE without a single error-severity diagnostic.
//
// This is the guard that was missing when bots/review-pr/main.bot shipped with
// `agent emit:` — a node named after the reserved `emit` node-type keyword
// (ADR-051), which the parser rejects with E002. That bot passed CI because the
// two tests that DO load every catalog bot both bail on a parse/compile failure
// ("another test's job") without ever failing:
//   - bundle_consistency_test.go: `if pr.File == nil { continue }`
//   - catalog_typing_test.go: only inspects four typing codes
//
// So a non-parseable bot sailed through CI and only surfaced in prod as a 502
// on the review-pr webhook (`launch failed: parse error … expected agent
// name`). This test IS that job: a catalog bot that a fresh `iterion validate`
// would reject must fail here, at build time, not at launch time.
//
// It asserts exactly what the launch path requires: parser.Parse must produce
// a File with no error diagnostics, and ir.Compile must produce a Workflow with
// no error diagnostics. Warnings are out of scope (the typing/consistency tests
// own those).
// teamBotFiles lists every workflow of the catalog, not only each bot's
// main.bot: a sibling workflow (extend.bot, reanchor.bot, sync-harness.bot)
// launches by the same path and must pass the same gates.
func teamBotFiles() ([]string, error) { return filepath.Glob("*/*.bot") }

func TestCatalogBotsParseAndCompileClean(t *testing.T) {
	teamBots, _ := teamBotFiles()
	demoMain, _ := filepath.Glob("../examples/*/main.bot")
	demoLoose, _ := filepath.Glob("../examples/*.bot")
	// The loose workflows shipped outside bots/ and examples/: the
	// dispatcher's `default` assignee, copied into the //go:embed tree at
	// build (a parse regression there breaks `iterion dispatch` in every
	// binary), and the operator scripts under scripts/adhoc/.
	shipped := []string{"../pkg/cli/templates/dispatch_bots_default.bot"}
	scripts, _ := filepath.Glob("../scripts/adhoc/*.bot")
	targets := append(append(append(append(teamBots, demoMain...), demoLoose...), shipped...), scripts...)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}

	checked := 0
	for _, path := range targets {
		// The bot's unit: its main and the fragments the imports reach.
		u := unit.LoadDir(path)
		parseFailed := false
		for _, d := range u.Diagnostics {
			if d.Severity == parser.SeverityError {
				parseFailed = true
				t.Errorf("%s: parse error: %s", path, d.Error())
			}
		}
		if u.Merged == nil {
			t.Errorf("%s: parser produced no File (unrecoverable parse failure)", path)
			continue
		}
		if parseFailed {
			continue // don't cascade compile errors off a broken parse
		}

		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Errorf("%s: compile produced no Workflow", path)
		}
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Errorf("%s: compile error: %s", path, d.Error())
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no catalog workflows parsed+compiled — discovery glob likely broke")
	}
}
