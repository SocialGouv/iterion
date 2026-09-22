package ir

import (
	"regexp"
	"regexp/syntax"
	"testing"
)

// TestC160_VarMatchingNonString: a pattern constrains TEXT, so it is
// string-only. `json` / `string[]` matter most here — they render
// differently on the default and the override path, so a pattern would
// certify one rendering while the run uses the other.
func TestC160_VarMatchingNonString(t *testing.T) {
	cases := []struct {
		varsLine string
		want     int
	}{
		{`  count: int [matching: "^[0-9]+$"]`, 1},
		{`  flag: bool [matching: "^t"]`, 1},
		{`  ratio: float [matching: "^1"]`, 1},
		{`  blob: json [matching: "^\\{"]`, 1},
		{`  tags: string[] [matching: "^a"]`, 1},
		{`  agent: string [matching: "^[a-z]+$"]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.varsLine, func(t *testing.T) {
			r := compileFile(t, varDefaultSrc(tc.varsLine))
			if got := countCode(r, DiagVarMatchingNonString); got != tc.want {
				t.Errorf("C160 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// TestC161_VarDefaultNotMatching: a default is checked on the literal text
// as written, exactly as C126 checks an enum default.
//
// The `${...}` case is the one that matters and it is deliberately an
// ERROR, not a skip: a default NEVER reaches the launch gate (the gate
// judges the operator's values), so a default excused here would be
// checked on no path at all while its declaration reads as constrained.
func TestC161_VarDefaultNotMatching(t *testing.T) {
	cases := []struct {
		varsLine string
		want     int
	}{
		{`  agent: string [matching: "^[a-z]+$"] = "Code X"`, 1},
		{`  agent: string [matching: "^[a-z]+$"] = "codex"`, 0},
		{`  agent: string [matching: "^[a-z]+$"]`, 0}, // no default, nothing to check
		{`  agent: string [matching: "^[a-z]+$"] = "${AGENT}"`, 1},
		{`  agent: string [matching: "^[a-z]+$"] = 5`, 0}, // wrong type → C109
	}
	for _, tc := range cases {
		t.Run(tc.varsLine, func(t *testing.T) {
			r := compileFile(t, varDefaultSrc(tc.varsLine))
			if got := countCode(r, DiagVarDefaultNotMatching); got != tc.want {
				t.Errorf("C161 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// TestC162_VarMatchingUncompilable: a pattern that is not RE2 is refused
// at compile time, and the IR carries no pattern — so nothing downstream
// can read a half-constraint as "the value passed".
func TestC162_VarMatchingUncompilable(t *testing.T) {
	r := compileFile(t, varDefaultSrc(`  agent: string [matching: "^[a-z"]`))
	if got := countCode(r, DiagVarMatchingUncompilable); got != 1 {
		t.Fatalf("C162 count = %d, want 1\ndiagnostics: %v", got, r.Diagnostics)
	}
	if r.Workflow != nil {
		if v := r.Workflow.Vars["agent"]; v != nil && v.Matching != "" {
			t.Errorf("an uncompilable pattern must not reach the IR, got %q", v.Matching)
		}
	}
	// A pattern that LOOKS well-formed is caught just the same: RE2
	// refuses a descending repeat count, and the author must hear it at
	// compile time rather than at the first launch.
	r2 := compileFile(t, varDefaultSrc(`  agent: string [matching: "^a{2,1}$"]`))
	if got := countCode(r2, DiagVarMatchingUncompilable); got != 1 {
		t.Errorf("an invalid repeat count must be C162, got %d\ndiagnostics: %v", got, r2.Diagnostics)
	}
}

// TestC163_VarMatchingUnanchored: Go matches by SEARCH, so an unanchored
// pattern also admits a value that merely CONTAINS a match — `[a-z]+`
// accepts `--flag`, one of the values #1350 exists to refuse. It is a
// WARNING, not a refusal: an author may genuinely want a substring rule,
// and refusing would be an artificial limit.
func TestC163_VarMatchingUnanchored(t *testing.T) {
	cases := []struct {
		varsLine string
		want     int
	}{
		{`  agent: string [matching: "[a-z]+"]`, 1},
		{`  agent: string [matching: "^[a-z]+"]`, 1},
		{`  agent: string [matching: "[a-z]+$"]`, 1},
		{`  agent: string [matching: "^a$|b"]`, 1},
		// Anchored under the m flag is NOT anchored: the anchors become
		// LINE anchors, so "abc\nrm -rf /" passes a pattern that reads as
		// if it could not.
		{`  agent: string [matching: "(?m)^[a-z]+$"]`, 1},
		{`  agent: string [matching: "^[a-z]+$"]`, 0},
		{`  agent: string [matching: "^(a|b)$"]`, 0},
		{`  agent: string [matching: "^$"]`, 0},
		// The dogfood declaration: an alternation with every branch anchored.
		{`  agent: string [matching: "^[A-Za-z0-9._][A-Za-z0-9._-]*$|^$"]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.varsLine, func(t *testing.T) {
			r := compileFile(t, varDefaultSrc(tc.varsLine))
			if got := countCode(r, DiagVarMatchingUnanchored); got != tc.want {
				t.Errorf("C163 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// TestUnanchoredPatternReallyAdmitsAFlag is the fact C163 asserts, checked
// against the regexp engine rather than restated: without it the warning
// would be an opinion about spelling.
func TestUnanchoredPatternReallyAdmitsAFlag(t *testing.T) {
	for _, value := range []string{"--flag", " codeX --some-flag", "!!!abc!!!"} {
		ok, err := ValueMatchesPattern(`[a-z]+`, value)
		if err != nil {
			t.Fatalf("ValueMatchesPattern: %v", err)
		}
		if !ok {
			t.Fatalf("fixture is inert: %q was expected to slip through the unanchored pattern", value)
		}
		// …and the anchored form refuses all three, which is what the
		// warning asks the author to move to.
		ok, err = ValueMatchesPattern(`^[a-z]+$`, value)
		if err != nil {
			t.Fatalf("ValueMatchesPattern: %v", err)
		}
		if ok {
			t.Errorf("the anchored pattern must refuse %q", value)
		}
	}
}

// TestPatternIsAnchoredReadsStructureNotSpelling: the check parses the
// regexp, so an author's spelling of the anchors is irrelevant. `\A…\z`
// is anchored though it contains no "^"; `(?m)^…$` is not, though it
// contains both.
func TestPatternIsAnchoredReadsStructureNotSpelling(t *testing.T) {
	cases := map[string]bool{
		`^[a-z]+$`:          true,
		`\A[a-z]+\z`:        true,
		`^(a|b)$`:           true,
		`^a$|^b$`:           true,
		`(?m)^[a-z]+$`:      false,
		`[a-z]+`:            false,
		`^[a-z]+`:           false,
		`(^a$)`:             true,
		`^https://.*$`:      true,
		`^[a-z]+$|^[A-Z]+$`: true,
	}
	for pattern, want := range cases {
		re, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("parse %q: %v", pattern, err)
		}
		if got := patternIsAnchored(re.Simplify()); got != want {
			t.Errorf("patternIsAnchored(%q) = %v, want %v", pattern, got, want)
		}
	}
}

// TestValueMatchesPatternIsTheOneReading: the compiler's default check and
// the runtime's launch gate must agree by construction, so both call this.
// A pattern that does not compile is an ERROR, never a silent "passed".
func TestValueMatchesPatternIsTheOneReading(t *testing.T) {
	ok, err := ValueMatchesPattern(`^[0-9]+$`, "42")
	if err != nil || !ok {
		t.Errorf("42 against ^[0-9]+$: ok=%v err=%v", ok, err)
	}
	ok, err = ValueMatchesPattern(`^[0-9]+$`, "4 2")
	if err != nil || ok {
		t.Errorf("'4 2' against ^[0-9]+$: ok=%v err=%v", ok, err)
	}
	ok, err = ValueMatchesPattern(`^[a-z`, "anything")
	if err == nil {
		t.Fatal("an uncompilable pattern must return an error, not a verdict")
	}
	if ok {
		t.Error("an uncompilable pattern must never report a match")
	}
}

// TestMatchingRidesTheContract: a reader of a bot's public contract must
// not be told a wider domain than the program accepts — the reason
// EnumValues is carried there, and the same reason for the pattern.
func TestMatchingRidesTheContract(t *testing.T) {
	r := compileFile(t, varDefaultSrc(`  agent: string [matching: "^[a-z]+$"] = "codex"`))
	if r.Workflow == nil {
		t.Fatal("workflow did not compile")
	}
	v := r.Workflow.Vars["agent"]
	if v == nil {
		t.Fatal("var \"agent\" is missing from the compiled workflow")
	}
	if v.Matching != `^[a-z]+$` {
		t.Fatalf("the IR var carries Matching = %q, want the pattern as declared", v.Matching)
	}
}

// TestPatternIsAnchoredAgreesWithABruteForceOracle.
//
// patternIsAnchored is a PREDICATE over a parsed regexp, so its claim is
// falsifiable against the regexp engine itself: a pattern is anchored
// exactly when it cannot match a PROPER SUBSTRING of a value it does not
// match whole. The oracle asks that directly — `re` against `\A(?:re)\z`
// over a corpus — so the check is held to a behaviour, never to a list of
// anchor spellings, which is the shape that does not converge.
//
// The costly direction is a false NEGATIVE: a pattern the check calls
// anchored that is not, because C163 then stays silent on the foot-gun it
// exists for. A false positive is only noise, and is asserted too.
func TestPatternIsAnchoredAgreesWithABruteForceOracle(t *testing.T) {
	patterns := []string{
		// anchored, in every spelling the repo's authors plausibly use
		`^[a-z]+$`, `\A[a-z]+\z`, `^(a|b)$`, `^a$|^b$`, `(^a$)`, `((^a$))`,
		`^$`, `^https://.*$`, `^[A-Za-z0-9._][A-Za-z0-9._-]*$|^$`,
		`^(?:dev|staging|prod)$`, `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`,
		// anchored with the anchors DISTRIBUTED — the shapes the first
		// implementation warned at
		`^(?:a$|b$)`, `^(a$|b$)`, `^(?:dev$|prod$)`, `(^a)($)`, `(^)(a)($)`,
		`^a$|(^b$)`, `\A(?:a|b)\z`, `^(?:a|b$)$`,
		// unanchored
		`[a-z]+`, `^[a-z]+`, `[a-z]+$`, `^a$|b`, `a|^b$`, `^https://`,
		`(?m)^[a-z]+$`, `(?m)^a$|^b$`, `x`, `^a|b$`,
		// degenerate
		`^`, `$`, `a*`, `^a*`, `(a|b)`,
	}
	corpus := []string{
		"", "a", "b", "x", "ab", "ba", "aa", "-a", "a-", " a", "a ", "a b",
		"dev", "prod", "staging", "xdevx", "--flag", " codeX --some-flag",
		"https://x", "yhttps://x", "2026-09-22", "z2026-09-22z",
		"a\nb", "\na", "a\n", "abc", "zabcz", "!!!abc!!!", "A1", "a.b-c",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			re, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("compile %q: %v", pattern, err)
			}
			whole, err := regexp.Compile(`\A(?:` + pattern + `)\z`)
			if err != nil {
				t.Fatalf("compile the whole-value form of %q: %v", pattern, err)
			}
			// Truth: the pattern is anchored when SEARCHING it and
			// requiring it to span the whole value are the same question.
			truth := true
			var witness string
			for _, v := range corpus {
				if re.MatchString(v) != whole.MatchString(v) {
					truth = false
					witness = v
					break
				}
			}
			parsed, err := syntax.Parse(pattern, syntax.Perl)
			if err != nil {
				t.Fatalf("parse %q: %v", pattern, err)
			}
			got := patternIsAnchored(parsed.Simplify())
			if got == truth {
				return
			}
			if truth {
				t.Errorf("patternIsAnchored(%q) = false, but the pattern DOES span every value it matches — C163 is noise here", pattern)
				return
			}
			t.Errorf("patternIsAnchored(%q) = true, but %q matches only a PART of it — C163 stays silent on the foot-gun it exists for",
				pattern, witness)
		})
	}
}

// TestC164_VarRedeclaredWithoutItsConstraint.
//
// A workflow-level `vars:` entry REPLACES the top-level one of the same
// name, so a redeclaration that omits the constraint deletes it — silently,
// and the launch gate then accepts anything. The duplicate is already an
// error INSIDE one block (E010); the rule simply stopped at the block
// boundary, leaving the worst failure a constraint can have: declared,
// visible in the file, and inert.
//
// Mutation that reddens it: drop the `prev` check from addVars.
func TestC164_VarRedeclaredWithoutItsConstraint(t *testing.T) {
	src := func(top, wf string) string {
		return "dsl: 2\n\nvars:\n  agent: " + top + "\n\nschema label:\n  kind: string\n\n" +
			"compute show:\n  output: label\n  expr:\n    kind: \"vars.agent\"\n\n" +
			"workflow w:\n  vars:\n    agent: " + wf + "\n  entry: show\n  show -> done\n"
	}
	cases := []struct {
		name    string
		top, wf string
		want    int
	}{
		{"pattern dropped", `string [matching: "^[a-z]+$"]`, `string`, 1},
		{"enum dropped", `string [enum: "a", "b"]`, `string`, 1},
		// One diagnostic, not one per bracket: the question is whether the
		// var is still constrained at all.
		{"both dropped", `string [enum: "a"] [matching: "^[a-z]+$"]`, `string`, 1},
		{"pattern repeated", `string [matching: "^[a-z]+$"]`, `string [matching: "^[a-z]+$"]`, 0},
		{"pattern tightened", `string [matching: "^[a-z]+$"]`, `string [matching: "^[a-z]$"]`, 0},
		{"enum repeated", `string [enum: "a", "b"]`, `string [enum: "a", "b"]`, 0},
		{"neither declared one", `string`, `string`, 0},
		// The redeclaration may ADD a constraint: only losing every one is refused.
		{"constraint added", `string`, `string [matching: "^[a-z]+$"]`, 0},
		// Which constraint is the author's business: an enum may become the
		// equivalent pattern, and comparing two patterns for equivalence is
		// undecidable — so a REPLACED constraint is not diagnosed.
		{"enum becomes a pattern", `string [enum: "a"]`, `string [matching: "^a$"]`, 0},
		{"pattern becomes an enum", `string [matching: "^a$"]`, `string [enum: "a"]`, 0},
		// The redeclaration DOES carry the constraint; it is refused for
		// another reason (C160 / C125 / C162). Reporting it as absent would
		// send the author to repeat a line already on the page, so C164 must
		// read what was WRITTEN, not what the compiler kept.
		{"pattern on a non-string redeclaration", `string [matching: "^a$"]`, `int [matching: "^a$"]`, 0},
		{"enum on a non-string redeclaration", `string [enum: "a"]`, `int [enum: "a"]`, 0},
		{"redeclared pattern does not compile", `string [matching: "^a$"]`, `string [matching: "^(a$"]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, src(tc.top, tc.wf))
			if got := countCode(r, DiagVarRedeclaredUnconstrained); got != tc.want {
				t.Errorf("C164 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}
