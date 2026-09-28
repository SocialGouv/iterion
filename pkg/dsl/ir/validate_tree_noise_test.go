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
// fires on trains authors to ignore it, so those spans are suppressed.
//
// Mutation that reddens it: drop the suppression call from the C158 scan.
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

// TestQuotedTreeNoiseEnvScannerCoverage: the four exact spellings were not
// the class — a defaulted braced form and a concatenation inside one quoted
// span collapse the same way. And the message must match the quote style:
// single quotes never expand at all (the literal text reaches the command),
// double quotes expand to ONE word. The AST keeps no per-property span (the
// lot-1b work), so the position travels in the message: the body-relative
// line of the offending span.
//
// Mutation that reddens it: reinstate the four-substring quotedTreeNoiseEnv.
func TestQuotedTreeNoiseEnvScannerCoverage(t *testing.T) {
	t.Run("a defaulted braced form", func(t *testing.T) {
		r := compileText(t, treeNoiseSrc("  command: `git status --porcelain -- \"${ITERION_TREE_NOISE:-}\"`"))
		got := treeNoiseDiag(r, DiagTreeNoiseEnvQuoted)
		if got == nil {
			t.Fatalf("no C158 on \"${ITERION_TREE_NOISE:-}\"\ndiagnostics: %v", r.Diagnostics)
		}
	})
	t.Run("a concatenation inside one quoted span", func(t *testing.T) {
		r := compileText(t, treeNoiseSrc("  command: `git status --porcelain -- \"prefix $ITERION_TREE_NOISE\"`"))
		got := treeNoiseDiag(r, DiagTreeNoiseEnvQuoted)
		if got == nil {
			t.Fatalf("no C158 on \"prefix $ITERION_TREE_NOISE\"\ndiagnostics: %v", r.Diagnostics)
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
