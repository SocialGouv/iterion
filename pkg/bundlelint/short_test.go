package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// An executable source the floor walk could not read whole draws C256, an
// error — what its unread files use is not known, and the push refuses the
// bundle — naming the source and the budget's variable.
func TestASourceCutShortDrawsC256(t *testing.T) {
	short := []string{"child.bot: lib/big.bot:3:1: error [E001]: the bot is past the read budget of 400 — " + parser.ReadBudgetHint}
	diags := CheckConsistency(Input{Manifest: &bundle.Manifest{Name: "probe"}, Syntax: bundle.SyntaxRequirements{Short: short}})
	var found *Diag
	for i := range diags {
		if diags[i].Code == DiagProfileSourceShort {
			found = &diags[i]
		}
	}
	if found == nil || found.Severity != SeverityError || !strings.Contains(found.Message, "child.bot") || !strings.Contains(found.Hint, parser.ReadBudgetEnv) {
		t.Fatalf("C256 = %+v in %v, want an error naming child.bot, its remedy the budget's variable", found, diags)
	}
	for _, d := range CheckConsistency(Input{Manifest: &bundle.Manifest{Name: "probe"}}) {
		if d.Code == DiagProfileSourceShort {
			t.Errorf("C256 drawn for a bundle read whole: %+v", d)
		}
	}
}
