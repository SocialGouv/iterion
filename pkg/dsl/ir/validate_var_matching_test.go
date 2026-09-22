package ir

import (
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
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

// presetSrc wraps a `vars:` block and a `presets:` block in the smallest
// file that compiles.
func presetSrc(varsLines, presetLines string) string {
	return "vars:\n" + varsLines + "\n\npresets:\n" + presetLines + `
prompt sys:
  hi

agent a:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  system: sys

workflow w:
  entry: a
  a -> done
`
}

// TestC165_PresetViolatesConstraint.
//
// A preset value is a literal written in the `.bot`, as checkable as a
// default and governed by the same constraint — but nothing checked it, so
// a bot could ship a preset that no launch can accept: `iterion validate`
// said OK and `iterion run --preset <name>` died at the gate (#1611).
//
// It is C165 rather than C126/C161 because the SITE differs, and the preset
// family already draws that line: a type mismatch on a default is C109, the
// same mismatch on a preset value is C071.
//
// Mutation that reddens it: drop the checkPresetConstraint call from
// compilePresets.
func TestC165_PresetViolatesConstraint(t *testing.T) {
	cases := []struct {
		name      string
		vars      string
		presets   string
		want      int
		wantInMsg string
	}{
		{
			name:      "enum: a value outside the set",
			vars:      `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets:   "  bad:\n    mode: \"yolo\"\n",
			want:      1,
			wantInMsg: "not one of the enum values",
		},
		{
			name:      "matching: a value off the pattern",
			vars:      `  agent: string [matching: "^[a-z]+$"] = "codex"`,
			presets:   "  bad:\n    agent: \"Code X\"\n",
			want:      1,
			wantInMsg: "does not match its pattern",
		},
		{
			// Both constraints are independent conjuncts here too: a value
			// inside the enum but off the pattern is still refused.
			name:      "in the enum, off the pattern",
			vars:      `  mode: string [enum: "fast", "slow-and-careful"] [matching: "^[a-z]+$"] = "fast"`,
			presets:   "  bad:\n    mode: \"slow-and-careful\"\n",
			want:      1,
			wantInMsg: "does not match its pattern",
		},
		{
			name:    "a conforming value",
			vars:    `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets: "  ok:\n    mode: \"slow\"\n",
			want:    0,
		},
		{
			// An unconstrained var accepts whatever the preset says: the
			// check must not start refusing values nothing declared.
			name:    "an unconstrained var",
			vars:    `  free: string = ""`,
			presets: "  ok:\n    free: \"anything at all\"\n",
			want:    0,
		},
		{
			// A non-string var cannot carry either constraint (C125/C160
			// zero them), so it reaches the "declares no constraint"
			// return. Kept as the witness that no C165 noise appears on an
			// ordinary typed preset.
			name:    "a non-string var",
			vars:    `  count: int = 1`,
			presets: "  ok:\n    count: 7\n",
			want:    0,
		},
		{
			// The comparison is EXACT — the launch gate does not trim, so
			// trimming here would bless a value the run then refuses.
			name:      "a leading space is not the enum value",
			vars:      `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets:   "  bad:\n    mode: \" fast\"\n",
			want:      1,
			wantInMsg: "not one of the enum values",
		},
		{
			// A value the expander REWRITES is not final: the run resolves
			// it, and the compile-time environment is not the launch
			// environment. Left to the gate, which judges the EXPANSION —
			// docs/dsl.md promises a preset that reading.
			name:    "a live reference is left to the gate",
			vars:    `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets: "  ci:\n    mode: \"${ITERION_PROBE_MODE:-slow}\"\n",
			want:    0,
		},
		{
			name:    "a bare live reference is left to the gate too",
			vars:    `  agent: string [matching: "^[a-z]+$"] = "codex"`,
			presets: "  ci:\n    agent: \"$AGENT_NAME\"\n",
			want:    0,
		},
		{
			// A `$` is not a reference. The expander never acts on these,
			// so the literal IS what the gate will see and the compiler can
			// still judge it — a rule spelled over the text shipped all of
			// them unchecked.
			name:      "a trailing dollar is not a reference",
			vars:      `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets:   "  bad:\n    mode: \"yolo$\"\n",
			want:      1,
			wantInMsg: "not one of the enum values",
		},
		{
			name:      "a forgotten closing brace is not a reference",
			vars:      `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets:   "  bad:\n    mode: \"yolo${\"\n",
			want:      1,
			wantInMsg: "not one of the enum values",
		},
		{
			// A key written twice keeps the LAST, and the last is legal:
			// the shadowed text is read by no run, so C165 says nothing.
			//
			// KNOWN GAP, recorded rather than decided: nothing says the
			// shadowed line is dead either. A duplicate var is E010, a
			// duplicate preset NAME is C072, a duplicate enum value is
			// C127 — a duplicate preset KEY is silence. Out of this
			// change's scope; the case is here so the silence is visible.
			name:    "a shadowed duplicate key raises no C165 (and nothing else warns about it)",
			vars:    `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets: "  probe:\n    mode: \"yolo\"\n    mode: \"fast\"\n",
			want:    0,
		},
		{
			// The mirror: the landing value is the bad one. Said ONCE —
			// a key written twice is one setting, not two.
			name:      "a duplicate key whose last value is bad is said once",
			vars:      `  mode: string [enum: "fast", "slow"] = "fast"`,
			presets:   "  probe:\n    mode: \"fast\"\n    mode: \"yolo\"\n",
			want:      1,
			wantInMsg: "not one of the enum values",
		},
		{
			// …and the offender is not always the first value of the
			// first preset: every value of every preset is judged.
			name:      "the fourth value of the third preset",
			vars:      `  mode: string [enum: "fast", "slow"] = "fast"` + "\n  a: string\n  b: string\n  c: string",
			presets:   "  one:\n    mode: \"fast\"\n  two:\n    mode: \"slow\"\n  three:\n    a: \"x\"\n    b: \"y\"\n    c: \"z\"\n    mode: \"yolo\"\n",
			want:      1,
			wantInMsg: "not one of the enum values",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, presetSrc(tc.vars, tc.presets))
			if got := countCode(r, DiagPresetViolatesConstraint); got != tc.want {
				t.Fatalf("C165 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
			if tc.wantInMsg == "" {
				return
			}
			var msg string
			for _, d := range r.Diagnostics {
				if d.Code == DiagPresetViolatesConstraint {
					msg = d.Message
					// A WARNING, never an error: an error blocks a run
					// that selects another preset or none, and strands a
					// paused run whose declaration was tightened — which
					// Engine.Run's gate deliberately protects against.
					if d.Severity != SeverityWarning {
						t.Errorf("C165 severity = %v, want warning — an error blocks runs this preset does not touch", d.Severity)
					}
				}
			}
			// The refusal names the preset, the var, the value and what it
			// failed: the author has to find one line in a file of them.
			for _, want := range []string{"preset ", tc.wantInMsg} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not carry %q", msg, want)
				}
			}
		})
	}
}

// TestC165_NamesTheOffendingPresetLine: a bot declares many presets, and
// the diagnostic has to point at the one value to edit — not at the vars
// block, and not at the first preset.
//
// Mutation that reddens it: emit through c.errorf instead of
// c.errorfAtSpan, so the diagnostic carries no position.
func TestC165_NamesTheOffendingPresetLine(t *testing.T) {
	src := presetSrc(
		`  mode: string [enum: "fast", "slow"] = "fast"`,
		"  first:\n    mode: \"fast\"\n  second:\n    mode: \"slow\"\n  third:\n    mode: \"yolo\"\n",
	)
	r := compileFile(t, src)
	var d *Diagnostic
	for i := range r.Diagnostics {
		if r.Diagnostics[i].Code == DiagPresetViolatesConstraint {
			d = &r.Diagnostics[i]
		}
	}
	if d == nil {
		t.Fatalf("no C165 raised\ndiagnostics: %v", r.Diagnostics)
	}
	if !strings.Contains(d.Message, `preset "third"`) {
		t.Errorf("message %q must name the offending preset, not another one", d.Message)
	}
	if d.Line == 0 {
		t.Fatalf("C165 carries no line: an editor and an agent have nowhere to jump\n%+v", d)
	}
	// The line is the preset VALUE's, so it lands on the text to change.
	lines := strings.Split(src, "\n")
	if d.Line > len(lines) || !strings.Contains(lines[d.Line-1], "yolo") {
		t.Errorf("C165 points at line %d (%q), want the line carrying \"yolo\"",
			d.Line, lines[min(d.Line, len(lines))-1])
	}

	// …and the guarantee stops at the TEXT path. The studio validates a
	// document that has been through the JSON AST, where a preset value
	// carries no span (jsonPresetValue has no field for one) — so C165
	// arrives there with nowhere to jump, as C070/C071/C072 already do.
	// Asserted rather than left implied: a test that only ever walks the
	// parser reads as if it covered both.
	data, err := ast.MarshalFile(parseFile(t, src))
	if err != nil {
		t.Fatalf("MarshalFile: %v", err)
	}
	back, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("UnmarshalFile: %v", err)
	}
	viaJSON := Compile(back)
	var viaDoc *Diagnostic
	for i := range viaJSON.Diagnostics {
		if viaJSON.Diagnostics[i].Code == DiagPresetViolatesConstraint {
			viaDoc = &viaJSON.Diagnostics[i]
			break
		}
	}
	if viaDoc == nil {
		t.Fatal("C165 is lost entirely through the JSON transport")
	}
	if viaDoc.Line != 0 {
		t.Logf("the JSON transport now carries a preset value's span (line %d) — tighten this test to require it", viaDoc.Line)
	}
}

// TestAValueWithoutADollarIsItsOwnExpansion is the property C165's skip
// rests on: the compiler judges a preset literal only when it can read it
// WHOLE, and "whole" is defined by the expander, not by a list of
// spellings. If a text carrying no `$` could ever expand to something
// else, the check would be judging a value the run does not use.
//
// Proven against the expander itself over a hostile corpus, with an
// expandFn that rewrites EVERY name it is asked about — so a text that
// comes back unchanged was never asked.
func TestAValueWithoutADollarIsItsOwnExpansion(t *testing.T) {
	loud := func(string) string { return "!!!REWRITTEN!!!" }
	for _, s := range []string{
		"", "fast", " fast", "slow-and-careful", "a b c", "{braces}", "%s", "\\$notadollar",
		"a\nb", "héllo", "100%", "{{vars.x}}", "[]", "${", "}", "a{b}c", "--flag", "path/to/x",
	} {
		if strings.ContainsRune(s, '$') {
			continue
		}
		if got := ExpandWithDefault(s, loud); got != s {
			t.Errorf("ExpandWithDefault(%q) = %q — a value with no $ must be its own expansion, or C165 judges text the run never uses", s, got)
		}
	}
	// The converse, so the skip is not vacuous: a value that DOES carry a
	// `$` is rewritten, which is exactly why it is left to the launch gate.
	if got := ExpandWithDefault("${NAME}", loud); got == "${NAME}" {
		t.Fatal("fixture is inert: the expander did not touch a ${...} value, so the skip proves nothing")
	}
}

// TestC165WarnsButNeverBlocks is the guarantee the severity carries, and
// the one that cost a round to find: a bad preset must not stop a program
// from compiling. An error there blocks a run that selects ANOTHER preset,
// a run that selects none — and it strands a paused run whose declaration
// was tightened after it started, which `Engine.Run`'s gate deliberately
// protects against and `resume --force` cannot reach, because a compile
// error is refused before the engine is ever asked.
//
// Mutation that reddens it: warnfAtSpan → errorfAtSpan in
// checkPresetConstraint.
func TestC165WarnsButNeverBlocks(t *testing.T) {
	r := compileFile(t, presetSrc(
		`  mode: string [enum: "fast", "slow"] = "fast"`,
		"  good:\n    mode: \"slow\"\n  other:\n    mode: \"yolo\"\n",
	))
	if countCode(r, DiagPresetViolatesConstraint) != 1 {
		t.Fatalf("fixture is inert: no C165 raised\ndiagnostics: %v", r.Diagnostics)
	}
	if r.HasErrors() {
		t.Errorf("a bad preset made the program fail to compile — a run selecting %q, or none, is blocked by it\ndiagnostics: %v",
			"good", r.Diagnostics)
	}
	if r.Workflow == nil {
		t.Fatal("no workflow compiled: the run cannot start at all")
	}
	// The good preset is still usable, values intact.
	if got := r.Workflow.Presets["good"].Values["mode"]; got != "slow" {
		t.Errorf("preset \"good\" carries mode = %#v, want \"slow\"", got)
	}
}

// TestCarriesLiveReferenceAsksTheExpander pins the rule C165's skip rests
// on to the expander's BEHAVIOUR, not to a spelling. A `$` is not a
// reference: the first six values below are left exactly as written by the
// run, so the compiler can still judge them — and a `strings.ContainsRune`
// rule shipped all six unchecked.
//
// Mutation that reddens it: carriesLiveReference → strings.ContainsRune(s, '$').
func TestCarriesLiveReferenceAsksTheExpander(t *testing.T) {
	inert := []string{"yolo$", "$", "yolo${", "$$", "100$", "yo$-lo", "yolo", ""}
	live := []string{"${A}", "${A:-yolo}", "$A", "a$b", "$1x", "yo$$lo"}

	loud := func(string) string { return "!!!REWRITTEN!!!" }
	for _, s := range inert {
		if carriesLiveReference(s) {
			t.Errorf("carriesLiveReference(%q) = true, but the expander leaves it alone", s)
		}
		// …and the expander agrees: that is what makes it checkable.
		if got := ExpandWithDefault(s, loud); got != s {
			t.Errorf("fixture wrong: ExpandWithDefault(%q) = %q", s, got)
		}
	}
	for _, s := range live {
		if !carriesLiveReference(s) {
			t.Errorf("carriesLiveReference(%q) = false, but the run rewrites it — judging it would judge text the run never uses", s)
		}
	}
}
