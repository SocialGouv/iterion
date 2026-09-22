package parser_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// varSrc wraps one `vars:` entry in the smallest file that parses.
//
// A second var follows the entry ON THE NEXT LINE, deliberately: a broken
// constraint's recovery swallows a line, and a fixture whose broken entry
// is followed by a BLANK line cannot see it — skipToNewline returns at once
// and every assertion passes on the shape of the fixture.
func varSrc(entry string) string {
	return "dsl: 2\n\nvars:\n  " + entry + "\n  kept: string = \"k\"\n\nschema label:\n  kind: string\n\n" +
		"compute show:\n  output: label\n  expr:\n    kind: \"'k'\"\n\n" +
		"workflow w:\n  entry: show\n  show -> done\n"
}

// varNames lists the vars a parse actually produced.
func varNames(f *ast.File) []string {
	if f == nil || f.Vars == nil {
		return nil
	}
	var out []string
	for _, v := range f.Vars.Fields {
		out = append(out, v.Name)
	}
	return out
}

func parseErrors(t *testing.T, src string) []string {
	t.Helper()
	res := parser.Parse("v.bot", src)
	var errs []string
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	return errs
}

// TestAVarDeclaresAtMostOneOfEachConstraint.
//
// Both plausible merges of a repeated bracket are wrong in the direction
// that matters: a second `[matching:]` DROPS the first pattern, and a
// second `[enum:]` WIDENS the accepted set. Either way the file reads as
// one constraint and the engine enforces another — the exact failure a
// constraint exists to prevent, and silent.
//
// The `[enum:]` half also guards a regression: before the constraint
// bracket became a loop, a second `[` was a parse error, so a generator or
// a hand-merge emitting two of them could not quietly widen an allow-list.
//
// Mutation that reddens it: drop the seenEnum / seenMatching guards from
// parseVarConstraints.
func TestAVarDeclaresAtMostOneOfEachConstraint(t *testing.T) {
	cases := []struct {
		entry string
		want  string // a substring the refusal must carry; "" = must parse
	}{
		{`x: string [enum: "a"] [enum: "b"]`, "at most one [enum:"},
		{`x: string [matching: "^a$"] [matching: "^b$"]`, "at most one [matching:"},
		{`x: string [enum: "a"] [matching: "^a$"] [enum: "b"]`, "at most one [enum:"},
		// One of each, in either order, is the supported shape.
		{`x: string [enum: "a", "b"] [matching: "^[a-z]$"]`, ""},
		{`x: string [matching: "^[a-z]$"] [enum: "a", "b"]`, ""},
		{`x: string [matching: "^[a-z]$"]`, ""},
		{`x: string [enum: "a", "b"]`, ""},
		{`x: string`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			errs := parseErrors(t, varSrc(tc.entry))
			if tc.want == "" {
				if len(errs) > 0 {
					t.Fatalf("a supported declaration was refused: %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("a repeated constraint was accepted in silence")
			}
			joined := strings.Join(errs, "; ")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("refusal %q does not say %q", joined, tc.want)
			}
		})
	}
}

// TestAMalformedConstraintDoesNotEatTheNextDeclaration.
//
// `expect` consumes on failure, so a constraint arm without recovery walks
// off the end of its line and swallows the `]`, the newline, the DEDENT and
// then tokens of whatever follows: one typo produced nine diagnostics, seven
// of them blaming innocent lines, and the next declaration disappeared from
// the program.
//
// Mutation that reddens it: remove the skipToNewline() recovery from the
// TokenEnum / TokenMatching arms of parseVarConstraints.
func TestAMalformedConstraintDoesNotEatTheNextDeclaration(t *testing.T) {
	for _, entry := range []string{
		`x: string [matching]`,
		`x: string [matching:]`,
		`x: string [matching: bare]`,
		`x: string [matching: 5]`,
		`x: string [matching: "^a$"`,
		`x: string [enum]`,
		`x: string [enum:]`,
		`x: string [foo: "a"]`,
		`x: string [`,
		`x: string []`,
		// The shapes whose offending token IS the line's end — the ones a
		// skipToNewline called from past the newline swallows.
		`x: string [matching:`,
		`x: string [matching: "^a$"`,
		`x: string [enum:`,
		`x: string [enum: "a"`,
		`x: string [enum: "a",`,
		`x: string [enum: "a" "b"]`,
		`x: string [matching: ""]`,
	} {
		t.Run(entry, func(t *testing.T) {
			res := parser.Parse("v.bot", varSrc(entry))
			// The declarations after the broken line must survive: the
			// program is still readable, and the operator is pointed at
			// the one line they got wrong.
			if res.File == nil {
				t.Fatal("the file did not parse at all")
			}
			// The var on the NEXT line is the witness: expect() consumes
			// on failure, so a recovery that skips to a newline it has
			// already passed eats the following declaration whole — one
			// typo, two declarations gone, and only the first named.
			if !slices.Contains(varNames(res.File), "kept") {
				t.Errorf("the var declared after the broken one was eaten by the error recovery (survivors: %v)",
					varNames(res.File))
			}
			if len(res.File.Computes) != 1 {
				t.Errorf("the compute declaration after the broken var is gone (%d found) — the error recovery ate it",
					len(res.File.Computes))
			}
			if len(res.File.Workflows) != 1 {
				t.Errorf("the workflow after the broken var is gone (%d found)", len(res.File.Workflows))
			}
		})
	}
}

// TestAMatchingPatternMustBeQuoted: a pattern is never a bare word
// (`^[a-z]+$` does not lex as one), so an unquoted token is a typo that
// would otherwise become a literal-text pattern admitting anything that
// contains it — a constraint that reads as a regex and is not one. Its
// bracket sibling `[enum: ...]` has always required quotes.
func TestAMatchingPatternMustBeQuoted(t *testing.T) {
	for _, entry := range []string{
		`x: string [matching: bare]`,
		`x: string [matching: enum]`,
		`x: string [matching: true]`,
		`x: string [matching: 5]`,
	} {
		t.Run(entry, func(t *testing.T) {
			errs := parseErrors(t, varSrc(entry))
			if len(errs) == 0 {
				t.Fatal("an unquoted pattern was accepted")
			}
			if !strings.Contains(strings.Join(errs, "; "), "quoted string") {
				t.Errorf("refusal %v must say the pattern has to be quoted", errs)
			}
		})
	}
}

// TestMatchingOnASchemaFieldIsRefusedByName: `matching` is var-only, and
// the likeliest way to arrive here is copying the documented vars line into
// a `schema` block — so the refusal says which construct accepts it and
// why, rather than "expected enum, got matching".
func TestMatchingOnASchemaFieldIsRefusedByName(t *testing.T) {
	src := "dsl: 2\n\nschema s:\n  f: string [matching: \"^a$\"]\n\n" +
		"compute a:\n  output: s\n  expr:\n    f: \"'a'\"\n\n" +
		"workflow w:\n  entry: a\n  a -> done\n"
	errs := parseErrors(t, src)
	if len(errs) == 0 {
		t.Fatal("a pattern on a schema field was accepted")
	}
	joined := strings.Join(errs, "; ")
	for _, want := range []string{"only valid on a `vars:` declaration", "schema field"} {
		if !strings.Contains(joined, want) {
			t.Errorf("refusal %q does not carry %q", joined, want)
		}
	}
}

// TestAnEmptyMatchingPatternIsRefused: `[matching: ""]` reads as a
// constraint and is none — RE2 matches by search, so the empty pattern is
// found in every value. Worse, the compiler treats an empty pattern as the
// UNCONSTRAINED state (so C160/C162/C163 never run) and `iterion fmt`
// erases the bracket: the declaration loses its own text with no
// diagnostic. Its bracket sibling refuses the degenerate form too
// (`[enum: ]` is an error).
//
// Mutation that reddens it: drop the pat.Value == "" arm.
func TestAnEmptyMatchingPatternIsRefused(t *testing.T) {
	errs := parseErrors(t, varSrc(`x: string [matching: ""]`))
	if len(errs) == 0 {
		t.Fatal("an empty pattern was accepted as a constraint")
	}
	joined := strings.Join(errs, "; ")
	for _, want := range []string{"constrains nothing", `"^$"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("refusal %q does not carry %q — it must say why, and what to write instead", joined, want)
		}
	}
}
