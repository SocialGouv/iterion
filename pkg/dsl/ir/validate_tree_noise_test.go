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
