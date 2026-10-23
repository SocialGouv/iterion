package parser

import (
	"errors"
	"strings"
	"testing"
)

// A source a bound of the reader refused whole has no reading, and Parse
// answers none: nil and a *ReadRefusal — the read budget's, positioned
// where it ran out, its Error naming the variable that moves it; the size
// cap's, at the head, with its own remedy. No exported door hands over the
// empty file such a source reads as; its one diagnostic is the refusal Parse
// answers. A source that reads — errors or not — is a result.
func TestAReadingTheReaderRefusedIsNoResult(t *testing.T) {
	t.Setenv(readBudgetEnv, "4096")
	src := listSource(80_000)
	pr, err := Parse("big.bot", src)
	var r *ReadRefusal
	if pr != nil || !errors.As(err, &r) {
		t.Fatalf("Parse of a source past the budget = %v, %v — want no result and a *ReadRefusal", pr, err)
	}
	if r.Bound != BoundBudget || r.Diagnostic.Code != DiagUnexpectedToken || r.Diagnostic.File != "big.bot" || r.Diagnostic.Line != 3 || !strings.Contains(r.Diagnostic.Message, "past the read budget of 4096") {
		t.Errorf("refusal = %+v, want the budget's, positioned in big.bot on the list's line", r)
	}
	if !strings.Contains(err.Error(), ReadBudgetEnv) || !strings.HasPrefix(err.Error(), r.Diagnostic.Error()) {
		t.Errorf("the refusal as an error = %q, want the positioned diagnostic and its remedy, %s", err, ReadBudgetEnv)
	}
	salvage := parseLenient("big.bot", src)
	if salvage.Refused != BoundBudget || salvage.ReadRefusal() == nil || len(salvage.Diagnostics) != 1 || salvage.Diagnostics[0] != r.Diagnostic {
		t.Errorf("the salvage of the same source: refused %v, refusal %v, diagnostics %v — want the empty reading, its one diagnostic the refusal Parse answers", salvage.Refused, salvage.ReadRefusal(), salvage.Diagnostics)
	}
	if salvage.File == nil || len(salvage.File.Agents) != 0 {
		t.Errorf("the salvage's file holds %+v, want the empty file", salvage.File)
	}

	t.Setenv(readBudgetEnv, "0")
	huge := strings.Repeat("#\n", maxSourceSize/2+1)
	pr, err = Parse("huge.bot", huge)
	if pr != nil || !errors.As(err, &r) || r.Bound != BoundSize || r.Diagnostic.Line != 1 || r.Diagnostic.Hint != sizeRefusalHint {
		t.Errorf("Parse of a source past the size cap = %v, %v — want no result and the size cap's refusal at the head, its remedy", pr, err)
	}

	for name, text := range map[string]string{
		"a source that reads":     "agent a:\n  model: \"m\"\n",
		"one the parser recovers": "agent a:\n  model: \"m\"\n  bogus: 1\n",
	} {
		pr, err := Parse("ok.bot", text)
		if err != nil || pr == nil || pr.ReadRefusal() != nil || pr.Refused != BoundNone {
			t.Errorf("%s: Parse = %v, %v — want a result", name, pr, err)
		}
	}
}
