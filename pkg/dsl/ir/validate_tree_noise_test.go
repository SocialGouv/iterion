package ir

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// C157 / C158 — the tree-noise channel in an executable body (#1555)
// ---------------------------------------------------------------------------

// treeNoiseSrc wraps a tool body in a minimal workflow. The %s carries the
// one line under test.
func treeNoiseSrc(tool string) string {
	return `prompt brief:
  Stage the pass's work, never the tree noise: {{run.tree_noise}}

tool gate:
  command: "true"
` + tool + `
workflow w:
  entry: gate
  gate -> done
`
}

// TestTreeNoiseRefInAnExecutableBody: a plain {{run.tree_noise}} in a
// command:, script: or postcondition: renders shell-escaped as ONE argument
// that matches no file — the exclusion vanishes in silence (the
// review-fanout scaffold shipped exactly that defect until #1530 caught it).
// The bang form and the unquoted env var are the legitimate spellings and
// stay silent; a prompt body is the member's own channel and stays silent.
//
// Mutation that reddens it: drop the DiagTreeNoiseRefInExecBody emission
// from validateTreeNoiseChannels.
func TestTreeNoiseRefInAnExecutableBody(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want bool // true = C157 fires
	}{
		"a plain ref in a command":        {`  command: "git add -A -- ':/' {{run.tree_noise}}"`, true},
		"a plain ref in a script":         {"  script: |\n    git add -A -- ':/' {{run.tree_noise}}\n  language: sh", true},
		"a plain ref in a postcondition":  {"  goal: \"g\"\n  postcondition: \"test -z \\\"$(git status --porcelain -- {{run.tree_noise}})\\\"\"", true},
		"the bang form in a command":      {`  command: "git add -A -- ':/' {{!run.tree_noise}}"`, false},
		"the unquoted env var":            {`  command: "git add -A -- ':/' $ITERION_TREE_NOISE"`, false},
		"a sub-field is C153's, not this": {`  command: "echo {{run.tree_noise.list}}"`, false},
		"another member entirely":         {`  command: "echo {{run.id}}"`, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			var got *Diagnostic
			for i := range r.Diagnostics {
				if r.Diagnostics[i].Code == DiagTreeNoiseRefInExecBody {
					got = &r.Diagnostics[i]
				}
			}
			if !tc.want {
				if got != nil {
					t.Fatalf("C157 on a sound body: %s", got.Message)
				}
				return
			}
			if got == nil || got.Severity != SeverityWarning {
				t.Fatalf("no C157 warning: %+v\n%v", got, r.Diagnostics)
			}
			if got.NodeID != "gate" {
				t.Errorf("C157 attributed to %q, want the tool node", got.NodeID)
			}
			for _, want := range []string{"{{!run.tree_noise}}", "$ITERION_TREE_NOISE"} {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q does not name the fix %q", got.Message, want)
				}
			}
		})
	}

	// The prompt's own channel stays silent: treeNoiseSrc's `brief` carries
	// the member rendered for an agent — the shape #1464 built — and no
	// diagnostic may fire on it.
	r := compileText(t, treeNoiseSrc(`  command: "git add -A -- ':/' $ITERION_TREE_NOISE"`))
	for _, d := range r.Diagnostics {
		if d.Code == DiagTreeNoiseRefInExecBody {
			t.Fatalf("C157 fired on a prompt rendering or a sound command: %s", d.Message)
		}
	}
}

// TestQuotedTreeNoiseEnvCollapsesTheList: "$ITERION_TREE_NOISE" quoted is one
// pathspec that matches nothing — git exits 0, the whole tree stages, and
// `validate --exec` reports the gate clean. Only a SHELL body is judged: in
// a js/py script the spelling is a literal string, not an env read.
func TestQuotedTreeNoiseEnvCollapsesTheList(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want bool // true = C158 fires
	}{
		"double-quoted in a command":   {"  command: `git status --porcelain -- \"$ITERION_TREE_NOISE\"`", true},
		"single-quoted in a command":   {"  command: `git add -A -- ':/' '$ITERION_TREE_NOISE'`", true},
		"braced and quoted":            {"  command: `git add -A -- \"${ITERION_TREE_NOISE}\"`", true},
		"quoted in a shell script":     {"  script: |\n    git status --porcelain -- \"$ITERION_TREE_NOISE\"\n  language: bash", true},
		"unquoted in a command":        {`  command: "git add -A -- ':/' $ITERION_TREE_NOISE"`, false},
		"quoted in a NON-shell script": {"  script: |\n    console.log(\"$ITERION_TREE_NOISE\")\n  language: js", false},
		"absent altogether":            {`  command: "git status --porcelain"`, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			var got *Diagnostic
			for i := range r.Diagnostics {
				if r.Diagnostics[i].Code == DiagTreeNoiseEnvQuoted {
					got = &r.Diagnostics[i]
				}
			}
			if !tc.want {
				if got != nil {
					t.Fatalf("C158 on a sound body: %s", got.Message)
				}
				return
			}
			if got == nil || got.Severity != SeverityWarning {
				t.Fatalf("no C158 warning: %+v\n%v", got, r.Diagnostics)
			}
			if !strings.Contains(got.Message, "word-split") {
				t.Errorf("message %q does not name the remedy", got.Message)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Adversarial-round findings on C157/C158
// ---------------------------------------------------------------------------

// c157 / c158 collect the diagnostics of one code from a compile.
func treeNoiseDiag(r *CompileResult, code DiagCode) *Diagnostic {
	for i := range r.Diagnostics {
		if r.Diagnostics[i].Code == code {
			return &r.Diagnostics[i]
		}
	}
	return nil
}

// TestTreeNoiseRefInANonShellScriptStaysSilent: a script: body renders its
// refs as JSON literals (resolveScriptTemplate, unconditional over
// languages), so `const noise = {{run.tree_noise}};` in a js/py body is
// VALID working code consuming the list as data — and both fixes the
// shell-body message names break it (the bang form splices raw pathspecs —
// a SyntaxError; a bare $ITERION_TREE_NOISE never env-expands in a script,
// expandBracedEnv answers ${NAME} only). Only a SHELL body (sh/bash/empty
// language, command:, postcondition:) has the one-argument defect.
//
// Mutation that reddens it: drop the b.shell gate from the C157 ref check.
func TestTreeNoiseRefInANonShellScriptStaysSilent(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want bool // true = C157 fires
	}{
		"a js body consumes the ref as data": {"  script: |\n    const noise = {{run.tree_noise}};\n    console.log(noise);\n  language: js", false},
		"a py body consumes the ref as data": {"  script: |\n    noise = {{run.tree_noise}}\n    print(noise)\n  language: py", false},
		"a sh body still fires":              {"  script: |\n    git add -A -- ':/' {{run.tree_noise}}\n  language: sh", true},
		"a bash body still fires":            {"  script: |\n    git add -A -- ':/' {{run.tree_noise}}\n  language: bash", true},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			got := treeNoiseDiag(r, DiagTreeNoiseRefInExecBody)
			if !tc.want {
				if got != nil {
					t.Fatalf("C157 on a non-shell script body: %s", got.Message)
				}
				return
			}
			if got == nil {
				t.Fatalf("no C157 on a shell script body\ndiagnostics: %v", r.Diagnostics)
			}
			// The script message names the mechanism that actually applies
			// there: a JSON literal, not shell escaping.
			if !strings.Contains(got.Message, "JSON") {
				t.Errorf("script message %q does not name the JSON-literal mechanism", got.Message)
			}
			for _, want := range []string{"{{!run.tree_noise}}", "$ITERION_TREE_NOISE"} {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q does not name the fix %q", got.Message, want)
				}
			}
		})
	}
}

// TestQuotedTreeNoiseEnvKeepsItsGuards: quoting the env var is MANDATORY
// where word-splitting would corrupt the test — `[ -n $VAR ]` unquoted is
// TRUE on an empty var (the one-word `-n`), and an assignment RHS never
// word-splits. A check whose remedy ("drop the quotes") breaks the guard it
// fires on trains authors to ignore it. Tests are suppressed by a BOUNDED
// region (the closing bracket, or the first separator for `test`),
// assignments by the standalone-word rule itself (`=` is not a word
// boundary, so `X="$V"` is never the named shape).
//
// Mutation that reddens it: test-region suppression removed from closeSpan,
// or `=` added to isShellBoundary.
func TestQuotedTreeNoiseEnvKeepsItsGuards(t *testing.T) {
	for name, line := range map[string]string{
		"a -n guard":                "  command: |\n    if [ -n \"$ITERION_TREE_NOISE\" ]; then\n      git add -A -- ':/' $ITERION_TREE_NOISE\n    fi",
		"a -z guard":                "  command: |\n    [ -z \"$ITERION_TREE_NOISE\" ] && exit 0",
		"a double-bracket guard":    "  command: |\n    if [[ -n \"$ITERION_TREE_NOISE\" ]]; then exit 1; fi",
		"a string equality test":    "  command: |\n    [ \"$ITERION_TREE_NOISE\" = \"\" ] && exit 0",
		"the test builtin":          "  command: |\n    test -n \"$ITERION_TREE_NOISE\" && git add -A -- ':/' $ITERION_TREE_NOISE",
		"an assignment RHS":         "  command: |\n    NOISE=\"$ITERION_TREE_NOISE\"\n    git status --porcelain -- $NOISE",
		"an export assignment":      "  command: |\n    export NOISE=\"$ITERION_TREE_NOISE\"",
		"a defaulted guard, braced": "  command: |\n    [ -n \"${ITERION_TREE_NOISE:-}\" ] || exit 0",
		// Measured on the catalogue (secured-renovacy's commit_changes): a
		// comment's lone apostrophe unbalanced a quote-pairing that crossed
		// lines, misframing the SOUND pattern below as one quoted span.
		"a comment's apostrophe does not unbalance the pairing": "  command: |\n    # the tool's stdout stays clean\n    if git status --porcelain -- ':/' $ITERION_TREE_NOISE ':(exclude)go' | grep -q .; then\n      git add -A -- ':/' $ITERION_TREE_NOISE >&2\n    fi",
		// A comment is prose: a quoted var in one is documentation, not a
		// shell expansion. (This is the case the comment skip exists for —
		// line-anchored pairing alone does not cover it.)
		"a comment carrying the quoted var": "  command: |\n    # do not write \"$ITERION_TREE_NOISE\" quoted; word-split it\n    git add -A -- ':/' $ITERION_TREE_NOISE",
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(line))
			if got := treeNoiseDiag(r, DiagTreeNoiseEnvQuoted); got != nil {
				t.Fatalf("C158 on a mandatory-quoting shape: %s", got.Message)
			}
		})
	}
}

// TestQuotedTreeNoiseEnvScannerCoverage: the unambiguous shape is a
// STANDALONE word that is exactly one quoted span holding exactly the var —
// the braced parameter-expansion forms included. Round 3 requalified the
// check to precision over recall: a lint that misses is acceptable, a lint
// that lies is not, so a concatenation inside one quoted span is now SILENT
// by design (round 2 fired on it — reversed; the intent of `"prefix $VAR"`
// cannot be told from data). The message must match the quote style:
// single quotes never expand at all (the literal text reaches the command),
// double quotes expand to ONE word. The AST keeps no per-property span (the
// lot-1b work), so the position travels in the message: the body-relative
// line of the offending span.
//
// Mutation that reddens it: treeNoiseEnvOnlyVar answering true for content
// beyond the exact var forms.
func TestQuotedTreeNoiseEnvScannerCoverage(t *testing.T) {
	t.Run("a defaulted braced form", func(t *testing.T) {
		r := compileText(t, treeNoiseSrc("  command: `git status --porcelain -- \"${ITERION_TREE_NOISE:-}\"`"))
		got := treeNoiseDiag(r, DiagTreeNoiseEnvQuoted)
		if got == nil {
			t.Fatalf("no C158 on \"${ITERION_TREE_NOISE:-}\"\ndiagnostics: %v", r.Diagnostics)
		}
	})
	t.Run("a concatenation inside one quoted span is silent by design", func(t *testing.T) {
		r := compileText(t, treeNoiseSrc("  command: `git status --porcelain -- \"prefix $ITERION_TREE_NOISE\"`"))
		if got := treeNoiseDiag(r, DiagTreeNoiseEnvQuoted); got != nil {
			t.Fatalf("C158 on a non-standalone shape: %s", got.Message)
		}
	})
	t.Run("the message matches the quote style and carries the body line", func(t *testing.T) {
		r := compileText(t, treeNoiseSrc("  command: |\n    echo ok\n    echo '$ITERION_TREE_NOISE'\n    git status --porcelain -- \"$ITERION_TREE_NOISE\""))
		var single, double *Diagnostic
		for i := range r.Diagnostics {
			d := &r.Diagnostics[i]
			if d.Code != DiagTreeNoiseEnvQuoted {
				continue
			}
			if strings.Contains(d.Message, "'$ITERION_TREE_NOISE'") {
				single = d
			}
			if strings.Contains(d.Message, `"$ITERION_TREE_NOISE"`) {
				double = d
			}
		}
		if single == nil || double == nil {
			t.Fatalf("want one C158 per quoted span, got %v", r.Diagnostics)
		}
		if !strings.Contains(single.Message, "never expand") {
			t.Errorf("single-quote message %q describes collapse mechanics — the var never expands at all", single.Message)
		}
		if strings.Contains(single.Message, "ONE word") {
			t.Errorf("single-quote message %q borrows the double-quote mechanism", single.Message)
		}
		if !strings.Contains(double.Message, "ONE word") {
			t.Errorf("double-quote message %q does not name the one-word collapse", double.Message)
		}
		if !strings.Contains(single.Message, "body line 2") || !strings.Contains(double.Message, "body line 3") {
			t.Errorf("messages must carry the body-relative line: single=%q double=%q", single.Message, double.Message)
		}
	})
}

// ---------------------------------------------------------------------------
// Round-3 probes — the suppression heuristics re-attacked
// ---------------------------------------------------------------------------

// TestTreeNoiseScannerRound3Probes pins the reviewer's probes against the
// requalified design: C158 names ONLY the standalone quoted word holding
// exactly the var, and the suppression regions are bounded — a `test`
// command ends at its separator, a bracket test at its closing `]` found
// with quote/comment awareness.
func TestTreeNoiseScannerRound3Probes(t *testing.T) {
	c158count := func(r *CompileResult) int { return countCode(r, DiagTreeNoiseEnvQuoted) }
	for name, tc := range map[string]struct {
		line string
		want int // number of C158 hits expected
	}{
		// p1: the bracket test suppresses ITS operand, and the region ends
		// at the closing ] — the git add span after && must still fire.
		"p1: a hit past the bracket test's close": {
			"  command: |\n    [ -n \"$ITERION_TREE_NOISE\" ] && git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		// p1b: a `test` command's region ends at ; — the hit after it fires.
		"p1b: a hit past the test command's separator": {
			"  command: |\n    test -n \"$ITERION_TREE_NOISE\"; git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		// p2/p28: a stray bracket inside an echo'd string or a comment is
		// neither an opener nor a closer — nothing is suppressed.
		"p2: a stray bracket in an echo'd string": {
			"  command: |\n    echo \"[x]\"; git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		"p28: a stray bracket in a comment": {
			"  command: |\n    # see [1] for the rationale\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		// p12: an argument, not an assignment — and not the standalone
		// shape either, so silent by design (git would refuse X=… LOUD,
		// which is not the silent-vanishing class).
		"p12: a name=value argument": {
			"  command: |\n    git add X=\"$ITERION_TREE_NOISE\"", 0},
		// p21: a multi-line double-quoted string is tracked ACROSS lines —
		// its interior is never scanned, and the hit on the later line
		// keeps its own line number.
		"p21: a multi-line double-quoted string": {
			"  command: |\n    MSG=\"don't quote the var\n    like $ITERION_TREE_NOISE here\"\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		// p22: a double-quoted spelling inside a multi-line SINGLE-quoted
		// string never expands at all — the inverted-mechanism FP.
		"p22: double quotes inside a multi-line single-quoted string": {
			"  command: |\n    MSG='multi\n    line with \"$ITERION_TREE_NOISE\" inside'\n    git add -A -- ':/' $ITERION_TREE_NOISE", 0},
		// p30: the '\'' idiom must not misframe the following span, and a
		// real hit on the next line still fires.
		"p30: the '\\'' idiom": {
			"  command: |\n    echo 'it'\\''s fine'\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		"p30b: the idiom wrapping a double-quoted var": {
			"  command: |\n    echo 'it'\\''s \"$ITERION_TREE_NOISE\"'", 0},
		// p31b: \$ is a literal dollar — it never expands.
		"p31b: an escaped dollar inside double quotes": {
			"  command: |\n    echo \"\\$ITERION_TREE_NOISE\"", 0},
		// An escaped quote closes nothing: without the \" skip the span
		// ends early, the next " opens one that never closes on the line,
		// and the open quote swallows the following line's real hit.
		"an escaped quote does not eat the next line's hit": {
			"  command: |\n    echo \"say \\\"hi\\\"\"\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		// p3: display text — the span holds more than the var; silent by
		// design (documented recall limit).
		"p3: echo display text": {
			"  command: |\n    echo \"excluded: $ITERION_TREE_NOISE\"", 0},
		// p4: a heredoc body is data, never argv; silent by design.
		"p4: a heredoc body": {
			"  command: |\n    cat <<EOF\n    \"$ITERION_TREE_NOISE\"\n    EOF", 0},
		// …and the shape the check exists for still fires, quoted and not.
		"the standalone double-quoted word": {
			"  command: |\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		"a hit after a heredoc ends": {
			"  command: |\n    cat <<EOF\n    done\n    EOF\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			if got := c158count(r); got != tc.want {
				t.Fatalf("C158 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}

	// p21 detail: the surviving hit is attributed to the git add line, not
	// to inside the string.
	r := compileText(t, treeNoiseSrc("  command: |\n    MSG=\"don't quote the var\n    like $ITERION_TREE_NOISE here\"\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\""))
	if d := treeNoiseDiag(r, DiagTreeNoiseEnvQuoted); d == nil || !strings.Contains(d.Message, "body line 3") {
		t.Fatalf("the hit must carry the git add line (3): %+v", d)
	}
}

// TestTreeNoiseRefInsideScriptQuotesIsData (p20): a sh script that wraps
// ANOTHER interpreter — python3 -c "… {{run.tree_noise}} …" — hands the ref
// to the shell as part of one quoted word whose content is SOURCE for that
// interpreter; rendered as a JSON literal it lands there as data, valid and
// working. C157's one-word verdict applies only to a ref the SHELL will
// word-read: outside quotes, outside heredocs.
//
// Mutation that reddens it: drop the firstFreeOccurrence gate from the
// script arm of the C157 check.
func TestTreeNoiseRefInsideScriptQuotesIsData(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want bool // true = C157 fires
	}{
		"a sh script wrapping python3, ref inside quotes": {
			"  script: |\n    python3 -c \"noise = {{run.tree_noise}}; print(len(noise.split()))\"\n  language: sh", false},
		"a sh script, ref bare": {
			"  script: |\n    git add -A -- ':/' {{run.tree_noise}}\n  language: sh", true},
		"a sh script, ref inside a heredoc": {
			"  script: |\n    cat <<EOF\n    {{run.tree_noise}}\n    EOF\n  language: sh", false},
		"a command keeps firing, quoted or not": {
			`  command: "git add -A -- ':/' {{run.tree_noise}}"`, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			got := treeNoiseDiag(r, DiagTreeNoiseRefInExecBody)
			if tc.want && got == nil {
				t.Fatalf("no C157\ndiagnostics: %v", r.Diagnostics)
			}
			if !tc.want && got != nil {
				t.Fatalf("C157 on a data-bound ref: %s", got.Message)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Round-4 probes — word-boundary sides, arithmetic, the case word
// ---------------------------------------------------------------------------

// TestTreeNoiseScannerRound4Probes pins the re-attack of the round-3
// rewrite: the closing side of a standalone word is wider than the opening
// side (redirection and subshell closers hand the collapsed word to argv
// exactly like a blank — measured at bash argc=1), arithmetic is a region
// with no word-splitting and no heredocs, and a case word never splits.
func TestTreeNoiseScannerRound4Probes(t *testing.T) {
	c158count := func(r *CompileResult) int { return countCode(r, DiagTreeNoiseEnvQuoted) }
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		// HIGH 1 — the closing side accepts >, < and ).
		"b1: redirect-glued closing": {
			"  command: |\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\">/dev/null", 1},
		"b2: paren-wrapped closing": {
			"  command: |\n    ( git add -A -- ':/' \"$ITERION_TREE_NOISE\")", 1},
		"b3: inside a command substitution": {
			"  command: |\n    OUT=$(git status --porcelain -- \"$ITERION_TREE_NOISE\")\n    echo \"$OUT\"", 1},
		// W7 — …but the OPENING side never gains them: a redirect target
		// is one word by design.
		"w7: a redirect target stays silent": {
			"  command: |\n    git add -A 2>\"$ITERION_TREE_NOISE\"", 0},
		// HIGH 2 — arithmetic is a region: << is a shift, no heredoc opens,
		// and a quoted var inside does not word-split either.
		"h2: an arithmetic shift does not open a heredoc": {
			"  command: |\n    echo $((1 << 2))\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		"h2b: an arithmetic command does not open a heredoc": {
			"  command: |\n    ((x << 2))\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		"h2c: a quoted var inside arithmetic": {
			"  command: |\n    echo $((n = \"$ITERION_TREE_NOISE\"))\n    git add -A -- ':/' \"$ITERION_TREE_NOISE\"", 1},
		// …and a real heredoc inside a command substitution still works.
		"heredoc inside $( ) is still consumed": {
			"  command: |\n    OUT=$(cat <<EOF\n    \"$ITERION_TREE_NOISE\"\n    EOF\n    )\n    git add -A -- ':/' $ITERION_TREE_NOISE", 0},
		// MEDIUM 1 — the case word never word-splits.
		"m1: a case word": {
			"  command: |\n    case \"$ITERION_TREE_NOISE\" in\n      \"\") exit 1 ;;\n    esac", 0},
		// …nor does a case pattern (the ( side stays closed for it).
		"a case pattern": {
			"  command: |\n    case x in\n      \"$ITERION_TREE_NOISE\") exit 1 ;;\n    esac", 0},
		// LOW — xargs re-splits downstream: the collapse never reaches argv.
		"a pipe into xargs": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | xargs git add --", 0},
		"a pipe into xargs with flags": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | xargs -r git add --", 0},
		// LOW — documented recall hole: a span inside another expansion.
		"a span inside another var's expansion": {
			"  command: |\n    echo ${OTHER:+\"$ITERION_TREE_NOISE\"}", 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			if got := c158count(r); got != tc.want {
				t.Fatalf("C158 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// TestTreeNoiseRefFiresOncePerBody (LOW): two occurrences of the same ref in
// one body are ONE finding — the message can only carry the first
// occurrence's line anyway, so the second diagnostic was a byte-identical
// duplicate (pre-existing since the check exists).
func TestTreeNoiseRefFiresOncePerBody(t *testing.T) {
	r := compileText(t, treeNoiseSrc("  command: |\n    git add -A -- ':/' {{run.tree_noise}}\n    git status --porcelain -- {{run.tree_noise}}"))
	if got := countCode(r, DiagTreeNoiseRefInExecBody); got != 1 {
		t.Fatalf("C157 count = %d, want 1 (deduped per body)\ndiagnostics: %v", got, r.Diagnostics)
	}
}

// ---------------------------------------------------------------------------
// Round-5 probes — the case region's `in`-gating, parens as command context,
// and the xargs suppression's bounds
// ---------------------------------------------------------------------------

// TestTreeNoiseScannerRound5Probes pins the re-attack of the case and xargs
// special cases: casePattern arms only from the "case word consumed,
// awaiting in" state; `(`/`$(` open a fresh command context; `esac` closes
// only at command position with a depth counter for nesting; and the xargs
// suppression is bounded to a double-quoted span whose producer is
// echo/printf feeding a flag-free xargs.
func TestTreeNoiseScannerRound5Probes(t *testing.T) {
	c158count := func(r *CompileResult) int { return countCode(r, DiagTreeNoiseEnvQuoted) }
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		// HIGH 1 — a bare `in` inside an arm is not `case … in`.
		"h1a: a for-list inside a case arm still fires": {
			"  command: |\n    case \"$MODE\" in\n      staged) for f in \"$ITERION_TREE_NOISE\"; do git add \"$f\"; done ;;\n    esac", 1},
		"h1b: a bare `in` argument inside an arm arms nothing": {
			"  command: |\n    case x in\n      a) grep -w in log.txt; git add -- \"$ITERION_TREE_NOISE\" ;;\n    esac", 1},
		// HIGH 2 — $( opens a fresh command context.
		"h2: a case word inside $( )": {
			"  command: |\n    OUT=$(case \"$ITERION_TREE_NOISE\" in \"\") exit 1 ;; esac)", 0},
		"h2b: a case word inside $( ) after a plain command": {
			"  command: |\n    echo $(case \"$ITERION_TREE_NOISE\" in \"\") exit 1 ;; esac)", 0},
		// MEDIUM 3 — paren-less patterns, nesting, esac-as-argument.
		"m3a: a paren-less case pattern inside $( )": {
			"  command: |\n    OUT=$(case x in \"$ITERION_TREE_NOISE\") echo m;; *) echo no;; esac)", 0},
		"m3b: an outer pattern after a nested case": {
			"  command: |\n    case a in\n      x) case \"$MODE\" in y) echo ok ;; esac ;;\n      \"$ITERION_TREE_NOISE\") git add -A ;;\n    esac", 0},
		"m3c: esac as an arm argument closes nothing": {
			"  command: |\n    case x in\n      a) echo esac ;;\n      \"$ITERION_TREE_NOISE\") exit 1 ;;\n    esac", 0},
		"m3d: a hit inside a nested case's body still fires": {
			"  command: |\n    case a in\n      x) case \"$MODE\" in y) git add -- \"$ITERION_TREE_NOISE\" ;; esac ;;\n    esac", 1},
		// MEDIUM 4 — the xargs suppression's bounds.
		"m4a: a single-quoted span feeding xargs is literal — fires": {
			"  command: |\n    echo '$ITERION_TREE_NOISE' | xargs git add --", 1},
		"m4b: a collapse upstream of the pipe fires": {
			"  command: |\n    git add -A -- \"$ITERION_TREE_NOISE\" | xargs echo staged", 1},
		// The exit round deleted the downstream parsing entirely: no flags
		// are read, so a non-re-splitting xargs is a documented MISS (the
		// honest direction) rather than a special case.
		"m4c: xargs -0 — silent since the exit (documented miss)": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | xargs -0 git add --", 0},
		"m4d: xargs -I{} — silent since the exit (documented miss)": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | xargs -I{} git add {}", 0},
		"m4e: xargs -d — silent since the exit (documented miss)": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | xargs -d ' ' git add --", 0},
		// LOW 5 — the pipe may end its line.
		"low5: a pipe continued on the next line": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" |\n      xargs git add --", 0},
		// …and the suppression itself survives its narrowing.
		"the plain echo | xargs shape still suppresses": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | xargs git add --", 0},
		"a printf producer still suppresses": {
			"  command: |\n    printf '%s\\n' \"$ITERION_TREE_NOISE\" | xargs git add --", 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			if got := c158count(r); got != tc.want {
				t.Fatalf("C158 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Exit round — the xargs special case is gone; ONE pipe rule remains
// ---------------------------------------------------------------------------

// TestTreeNoisePipeRule pins the exit: a DOUBLE-quoted span produced by
// echo/printf whose command ends at a pipe is silent — piped output: the
// downstream may re-split, and the check cannot tell. Nothing downstream is
// parsed (not the command, not flags, not continuations, not comments), so
// every shape the xargs machinery missed converges to the same verdict. A
// span whose producer consumes its argv (git add upstream of the pipe)
// keeps firing — the collapse happens before the pipe.
func TestTreeNoisePipeRule(t *testing.T) {
	c158count := func(r *CompileResult) int { return countCode(r, DiagTreeNoiseEnvQuoted) }
	for name, tc := range map[string]struct {
		line string
		want int
	}{
		"echo | backslash-continuation to anything": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | \\\n      xargs git add --", 0},
		"echo | env xargs": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | env xargs git add --", 0},
		"command echo | xargs": {
			"  command: |\n    command echo \"$ITERION_TREE_NOISE\" | xargs git add --", 0},
		"echo | comment continuation": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" | # stage the tree\n      xargs git add --", 0},
		"echo with a preceding argument still suppresses": {
			"  command: |\n    echo staged: \"$ITERION_TREE_NOISE\" | xargs git add --", 0},
		"echo \"$V\" extra args still suppress": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" v2 | xargs git add --", 0},
		"git add upstream of the pipe FIRES": {
			"  command: |\n    git add -A -- \"$ITERION_TREE_NOISE\" | xargs echo staged", 1},
		"echo || is not a pipe": {
			"  command: |\n    echo \"$ITERION_TREE_NOISE\" || exit 1", 1},
		"a single-quoted span feeding a pipe is literal": {
			"  command: |\n    echo '$ITERION_TREE_NOISE' | xargs git add --", 1},
		"an unknown producer keeps firing": {
			"  command: |\n    show \"$ITERION_TREE_NOISE\" | xargs git add --", 1},
		"an env-modified producer suppresses": {
			"  command: |\n    env echo \"$ITERION_TREE_NOISE\" | xargs git add --", 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, treeNoiseSrc(tc.line))
			if got := c158count(r); got != tc.want {
				t.Fatalf("C158 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// TestTreeNoiseCaseStack pins the per-depth case state: an inner case (even
// inside the outer case's WORD) neither consumes the outer compound's
// awaiting-in nor strands its pattern state.
func TestTreeNoiseCaseStack(t *testing.T) {
	c158count := func(r *CompileResult) int { return countCode(r, DiagTreeNoiseEnvQuoted) }
	// (a) the outer pattern survives an inner case inside the case word.
	r := compileText(t, treeNoiseSrc("  command: |\n    case $(case x in y) echo z;; esac) in\n      \"$ITERION_TREE_NOISE\") git add -A ;;\n    esac"))
	if got := c158count(r); got != 0 {
		t.Fatalf("(a) outer pattern after an inner case in the case word: %d hits, want 0\n%v", got, r.Diagnostics)
	}
	// (b) a real collapse in the outer arm AFTER the inner esac fires again.
	r = compileText(t, treeNoiseSrc("  command: |\n    case a in\n      x) case \"$MODE\" in y) echo ok ;; esac\n         git add -- \"$ITERION_TREE_NOISE\" ;;\n    esac"))
	if got := c158count(r); got != 1 {
		t.Fatalf("(b) outer-arm collapse after the inner esac: %d hits, want 1\n%v", got, r.Diagnostics)
	}
}
