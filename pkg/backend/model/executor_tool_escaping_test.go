package model

import (
	"os"
	"os/exec"
	"path/filepath"
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
			resolved := resolveCommandTemplate(command, []*ir.Ref{ref}, nil, map[string]any{"branch": tc.value}, nil)
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
	resolved := resolveCommandTemplate(`BRANCH={{!vars.branch}} true`, []*ir.Ref{ref}, nil, map[string]any{"branch": "x;id"}, nil)
	if !strings.Contains(resolved, "BRANCH=x;id") {
		t.Fatalf("raw ref was escaped after all — the catalog rule against it would be pointless: %s", resolved)
	}
}

// The escaping holds for the BARE shape above — and breaks BOTH ways when the
// bot author wraps the ref in quotes of their own. Prose would not settle
// which; the sentinel and the interpreter do.
//
// The engine cannot fix this at substitution time without guessing the
// author's quoting context, so the fix lives where the shape is written:
// diagnostic C137 (ir.refInQuotes) flags it at compile, and the catalog
// carries none.
func TestAuthorQuotedRefsAreNotContained(t *testing.T) {
	ref := &ir.Ref{Kind: ir.RefVars, Path: []string{"base_ref"}, Raw: "{{vars.base_ref}}"}

	t.Run("single quotes cancel — the value becomes syntax", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "pwned")
		resolved := resolveCommandTemplate(`BASE_REF='{{vars.base_ref}}' true`, []*ir.Ref{ref}, nil,
			map[string]any{"base_ref": "x;touch " + sentinel + ";#"}, nil)
		_ = exec.Command("sh", "-c", resolved).Run()
		if _, err := os.Stat(sentinel); err != nil {
			t.Errorf("no sentinel (%v) — if the runtime learned to see the author's quotes, C137 can be reconsidered; resolved: %s", err, resolved)
		}
	})

	t.Run("double quotes corrupt a benign value", func(t *testing.T) {
		resolved := resolveCommandTemplate(`BASE_REF="{{vars.base_ref}}" sh -c 'printf %s "$BASE_REF"'`,
			[]*ir.Ref{ref}, nil, map[string]any{"base_ref": "main"}, nil)
		out, err := exec.Command("sh", "-c", resolved).Output()
		if err != nil {
			t.Fatalf("resolved command failed (%v): %s", err, resolved)
		}
		if got := string(out); got == "main" {
			t.Errorf("value arrived clean — the containment claim C137 was narrowed on would hold after all; resolved: %s", resolved)
		} else {
			t.Logf("value arrived as %q, not %q — the runtime's quotes survive as data", got, "main")
		}
	})

	t.Run("double quotes inject when the value carries one", func(t *testing.T) {
		sentinel := filepath.Join(t.TempDir(), "pwned")
		resolved := resolveCommandTemplate(`BASE_REF="{{vars.base_ref}}" true`, []*ir.Ref{ref}, nil,
			map[string]any{"base_ref": `a";touch ` + sentinel + `;"b`}, nil)
		_ = exec.Command("sh", "-c", resolved).Run()
		if _, err := os.Stat(sentinel); err != nil {
			t.Errorf("no sentinel (%v) — resolved: %s", err, resolved)
		}
	})
}
