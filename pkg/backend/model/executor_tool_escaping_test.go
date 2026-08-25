package model

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The contract that makes every `VAR={{vars.x}} python3 -c "…"` in the bot
// catalog safe: a tool command's refs are SHELL-ESCAPED at substitution time.
// Read as source, those commands look unquoted, and that reading has already
// produced a confident (and wrong) "arbitrary command execution" report — the
// author of a bot cannot be asked to re-derive the answer, so it is pinned
// here, by EXECUTION.
//
// Removing the escaping turns this red on the sentinel, not on style.
func TestToolCommandRefsAreShellEscaped(t *testing.T) {
	ref := &ir.Ref{Kind: ir.RefVars, Path: []string{"branch"}, Raw: "{{vars.branch}}"}
	// The command shape the catalog actually writes: a bare env assignment in
	// front of an interpreter, no author-supplied quoting.
	const command = `BRANCH={{vars.branch}} sh -c 'printf %s "$BRANCH"'`

	for _, tc := range []struct{ name, value string }{
		{"command separator", "x;touch /tmp/iterion-escape-sentinel;#"},
		{"command substitution", "x$(touch /tmp/iterion-escape-sentinel)"},
		{"backticks", "x`touch /tmp/iterion-escape-sentinel`"},
		{"single quote", "x'y"},
		{"renovate group parens", "renovate/npm-(non-major)"},
		{"redirect", "x>/tmp/iterion-escape-sentinel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolveCommandTemplate(command, []*ir.Ref{ref}, nil, map[string]any{"branch": tc.value})
			if strings.Contains(resolved, "{{vars.branch}}") {
				t.Fatalf("ref was not substituted at all: %s", resolved)
			}
			// The shell is the oracle: run the resolved line and read back what
			// the interpreter actually received in the variable. Anything other
			// than the value verbatim means a byte escaped its word.
			out, err := exec.Command("sh", "-c", resolved).Output()
			if err != nil {
				t.Fatalf("resolved command failed (%v): %s", err, resolved)
			}
			if got := string(out); got != tc.value {
				t.Errorf("shell received %q, want the value verbatim %q — a byte broke out of its word\nresolved: %s", got, tc.value, resolved)
			}
		})
	}
}

// The escape hatch is real and must stay visibly dangerous: `{{!vars.x}}`
// substitutes verbatim. No catalog bot uses it (enforced by
// bots.TestCatalogHasNoRawRefs); this pins WHY that rule exists.
func TestRawRefsBypassEscapingByDesign(t *testing.T) {
	ref := &ir.Ref{Kind: ir.RefVars, Path: []string{"branch"}, Raw: "{{!vars.branch}}", Unquoted: true}
	resolved := resolveCommandTemplate(`BRANCH={{!vars.branch}} true`, []*ir.Ref{ref}, nil, map[string]any{"branch": "x;id"})
	if !strings.Contains(resolved, "BRANCH=x;id") {
		t.Fatalf("raw ref was escaped after all — the catalog rule against it would be pointless: %s", resolved)
	}
}
