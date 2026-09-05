package bots

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// phase2TypingCodes are the static cross-node typing diagnostics. No shipped
// catalog bot may trip them: an enum typo in a routing condition (C121), an
// incompatible comparison (C107), or a bare-numeric when-expression (C108) is
// a real bug, caught here the same way the repo-/stack-agnostic guards catch
// overfit. Errors are unconditional; warnings may be allowlisted as debt.
var phase2TypingCodes = map[ir.DiagCode]bool{
	ir.DiagEnumLiteralMismatch:     true, // C121 (error)
	ir.DiagExprOperandTypeMismatch: true, // C107 (warning)
	ir.DiagWhenExprNotBoolish:      true, // C108 (warning)
	ir.DiagVarDefaultTypeMismatch:  true, // C109 (error)
}

// typingAllowlist records (bot dir, code) warnings accepted as debt. Keep it
// SHRINKING. Errors (C121) must never be allowlisted. Currently empty.
var typingAllowlist = map[string]map[ir.DiagCode]bool{}

// TestCatalogBotsNoTypingRegressions compiles every catalog workflow (bundles
// under bots/ + loose .bot files under examples/) and asserts none trips a
// Phase-2 typing diagnostic. Prompts are not merged: the typing checks read
// only schemas, edges and compute expressions, so an unresolved bundle prompt
// is irrelevant here.
func TestCatalogBotsNoTypingRegressions(t *testing.T) {
	teamBots, _ := teamBotFiles()
	demoMain, _ := filepath.Glob("../examples/*/main.bot")
	demoLoose, _ := filepath.Glob("../examples/*.bot")
	targets := append(append(teamBots, demoMain...), demoLoose...)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found")
	}

	checked := 0
	for _, path := range targets {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", path, err)
			continue
		}
		pr := parser.Parse(path, string(src))
		if pr.File == nil {
			continue
		}
		cr := ir.Compile(pr.File)
		checked++

		bot := filepath.Base(filepath.Dir(path))
		for _, d := range cr.Diagnostics {
			if !phase2TypingCodes[d.Code] {
				continue
			}
			if d.Severity != ir.SeverityError && typingAllowlist[bot][d.Code] {
				continue
			}
			t.Errorf("%s: %s\n  (fix the expression; if accepted debt, add a typingAllowlist entry — never for an error)", path, d.Error())
		}
	}
	if checked == 0 {
		t.Fatal("no catalog workflows compiled — discovery glob likely broke")
	}
}
