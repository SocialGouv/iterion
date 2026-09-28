package author

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A raw line break other than LF inside a quoted value — a lone CR, a NEL,
// an LS or a PS byte — is folded to a space by yaml.v3: the value the
// document shows is not the value the program reads, and nothing else said
// so (#1814).
func TestARawFoldInsideAQuotedValueIsRefused(t *testing.T) {
	nl := string(rune(10))
	for _, tc := range []struct {
		name  string
		fold  string
		folds bool
	}{
		{"a raw NEL folds to a space", "\u0085", true},
		{"a lone CR folds to a space", "\r", true},
		{"an LS is kept as written", "\u2028", false},
		{"a PS is kept as written", "\u2029", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Join([]string{
				"dsl: 2",
				"nodes:",
				"  - tool: t",
				"    command: \"echo a" + tc.fold + "b\"",
				"workflow:",
				"  name: w",
				"  entry: t",
				"  edges:",
				"    - t -> done",
			}, nl) + nl
			res := Parse("x.yaml", []byte(src))
			if !tc.folds {
				return
			}
			if !res.HasErrors() {
				t.Fatalf("the raw fold inside the quoted value is accepted:%s%s", nl, src)
			}
			found := false
			for _, d := range res.Diagnostics {
				if strings.Contains(d.Message, "raw CR or NEL byte") {
					found = true
				}
			}
			if !found {
				t.Fatalf("the refusal does not name the fold:%s%v", nl, res.Diagnostics)
			}
		})
	}
	t.Run("a fold byte after the closing quote is not a fold", func(t *testing.T) {
		// The scan stops at the value's closing quote: a raw CR or NEL past
		// it belongs to the rest of the line the yaml reader handles its own
		// way (a line terminator, a comment) - it is no fold of the value.
		g := &guard{name: "x", rawLines: []string{"command: \"echo hi\"\r  more: x"}}
		n := &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Line: 1, Column: 10}
		g.rawFoldBreak(n)
		if len(g.diags) != 0 {
			t.Errorf("the terminator after the close is named a fold: %v", g.diags)
		}
		g2 := &guard{name: "x", rawLines: []string{"command: \"echo a\rb\""}}
		n2 := &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Line: 1, Column: 10}
		g2.rawFoldBreak(n2)
		if len(g2.diags) == 0 {
			t.Errorf("the fold inside the value is not refused")
		}
	})

	t.Run("an escaped newline is the newline the author wrote", func(t *testing.T) {
		src := strings.Join([]string{
			"dsl: 2",
			"nodes:",
			"  - tool: t",
			`    command: "echo a\nb"`,
			"workflow:",
			"  name: w",
			"  entry: t",
			"  edges:",
			"    - t -> done",
		}, nl) + nl
		res := Parse("x.yaml", []byte(src))
		if res.HasErrors() {
			t.Fatalf("the escape the author wrote is refused: %v", res.Diagnostics)
		}
		for _, d := range res.Diagnostics {
			if strings.Contains(d.Message, "raw CR or NEL byte") {
				t.Errorf("an escaped newline warns as a raw fold: %q", d.Message)
			}
		}
	})
}

// The remedies of #1814's lows: a profile the build does not read names the
// ones it does, and a number takes the sign's refusal in its own name.
func TestTheRemediesNameTheThingThatExists(t *testing.T) {
	nl := string(rune(10))
	t.Run("dsl: 1_0 names the profiles of this build", func(t *testing.T) {
		src := "dsl: 1_0" + nl
		res := Parse("x.yaml", []byte(src))
		if !res.HasErrors() {
			t.Fatalf("the broken profile header is accepted")
		}
		found := false
		for _, d := range res.Diagnostics {
			if strings.Contains(d.Message, "reads profiles 1 to 2") {
				found = true
			}
			if strings.Contains(d.Message, "write `10`") {
				t.Errorf("the remedy still names the profile that does not exist: %q", d.Message)
			}
		}
		if !found {
			t.Fatalf("no remedy names the existing profiles:%s%v", nl, res.Diagnostics)
		}
	})
	t.Run("a negative number is not called an integer", func(t *testing.T) {
		src := strings.Join([]string{
			"dsl: 2",
			"workflow:",
			"  name: w",
			"  budget:\n    max_cost_usd: -1",
			"  entry: done",
		}, nl) + nl
		res := Parse("x.yaml", []byte(src))
		if !res.HasErrors() {
			t.Fatalf("the negative cap is accepted")
		}
		found := false
		for _, d := range res.Diagnostics {
			if strings.Contains(d.Message, "takes a non-negative number") {
				found = true
			}
			if strings.Contains(d.Message, "non-negative integer") {
				t.Errorf("the message still names the wrong type: %q", d.Message)
			}
		}
		if !found {
			t.Fatalf("no refusal names a number:%s%v", nl, res.Diagnostics)
		}
	})
}

// One mistake, one message: a refused criterion parameter does not hand
// the compiler a second refusal of its own — the entry goes with its
// header, as a preset whose one value is refused goes with its (#1814).
func TestARefusedCriterionParameterDoesNotAddACompileError(t *testing.T) {
	nl := string(rune(10))
	src := strings.Join([]string{
		"dsl: 2",
		"vars:",
		"  goal: string",
		"contracts:",
		"  c:",
		"    inputs:",
		"      goal:",
		"        type: string",
		"    criteria:",
		"      ok:",
		"        kind: min_length",
		"        port: input.goal",
		"        params: {min: True}",
		"workflow:",
		"  name: w",
		"  entry: done",
	}, nl) + nl
	res := Parse("x.yaml", []byte(src))
	var errs []string
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(res.File.Contracts) == 0 || len(errs) == 0 {
		t.Fatalf("the refused value is not refused: %v", res.Diagnostics)
	}
	for _, d := range ir.Compile(res.File).Diagnostics {
		if d.Severity == ir.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	for _, e := range errs {
		if strings.Contains(e, "C302") || strings.Contains(e, "requires parameter") {
			t.Fatalf("the compile adds a second refusal for the one mistake:%s%s", nl, strings.Join(errs, nl))
		}
	}
}
