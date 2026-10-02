package bots_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// deepsecAgentCorpus spans what an operator actually mistypes on
// `--var deepsec_agent=...`, plus the values that must keep working.
// The empty string is the declared default — "no deep scan" — and the
// leading-dash forms are the ones the node's comment says it exists to
// refuse: they reach deepsec as a second FLAG, so the agent silently
// becomes whatever follows.
var deepsecAgentCorpus = []string{
	"", "codex", "claude_code", "claude-code", "a.b", "a_b", "A1", "x-y-z",
	"-", "-codex", "--some-flag", "--agent",
	" codex", "codex ", "co dex", "codex;rm -rf /", "code*", "code?", "$(id)",
	"`id`", "a|b", "a&b", "a>b", "a\nb", "café", "agent\t", "'q'", `"q"`,
}

// TestTheDeclaredPatternAndTheShellBeltAgreeOnDeepsecAgent holds the two
// guards of `deepsec_agent` to the same verdict on the same values.
//
// #1350 moved the primary refusal to the DSL — `[matching: ...]` on the var
// declaration, enforced at launch while the operator is still at the
// keyboard — and left the node's own `case` statement in place as a belt.
// Two guards for one value is exactly the shape that drifts: one gets
// widened, the other does not, and a bot ships documenting a rule it no
// longer enforces. So neither is restated here — the pattern is read from
// the COMPILED var and the belt is EXECUTED from the bot's own source.
//
// Mutation that reddens it: widen the declaration to the charset-only form
// the ticket sketched (`^[A-Za-z0-9._-]*$`), which admits a leading dash —
// the `-` is a literal at the end of a character class — while the belt
// still refuses it.
func TestTheDeclaredPatternAndTheShellBeltAgreeOnDeepsecAgent(t *testing.T) {
	src, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read the bot: %v", err)
	}

	// The declaration, asked of the compiler rather than matched out of
	// the text: what the engine enforces is the compiled var.
	pr := parser.Parse("sec-audit-source/main.bot", string(src))
	res := ir.Compile(pr.File)
	if res.Workflow == nil {
		t.Fatalf("the bot does not compile: %v", res.Diagnostics)
	}
	decl := res.Workflow.Vars["deepsec_agent"]
	if decl == nil {
		t.Fatal("the bot declares no deepsec_agent var")
	}
	if decl.Matching == "" {
		t.Fatal("deepsec_agent declares no [matching: ...] pattern — the launch-time refusal #1350 added is gone")
	}

	belt := extractShellGuard(t, string(src))

	for _, value := range deepsecAgentCorpus {
		t.Run(strings.ReplaceAll(value, "\n", "\\n"), func(t *testing.T) {
			declAdmits, err := ir.ValueMatchesPattern(decl.Matching, value)
			if err != nil {
				t.Fatalf("the declared pattern does not compile: %v", err)
			}
			beltAdmits := runShellGuard(t, belt, value)
			if declAdmits != beltAdmits {
				t.Errorf("the two guards disagree on %q: declaration %q admits=%v, the node's belt admits=%v",
					value, decl.Matching, declAdmits, beltAdmits)
			}
		})
	}
}

// extractShellGuard lifts the node's own `case "$DS_AGENT" in ... esac`
// out of the bot's source. Taking the real text is the point: a guard
// retyped into a test proves the test agrees with itself.
func extractShellGuard(t *testing.T, src string) string {
	t.Helper()
	const open = `case "$DS_AGENT" in`
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatalf("the node's %s guard is gone — if it was removed on purpose, this test goes with it", open)
	}
	rest := src[i:]
	j := strings.Index(rest, "esac")
	if j < 0 {
		t.Fatal("the DS_AGENT case statement has no esac")
	}
	return rest[:j+len("esac")]
}

// runShellGuard executes the extracted belt under `sh` with DS_AGENT set,
// and reports whether it ADMITS the value. err_envelope is stubbed to a
// non-zero exit: in the bot it writes a refusal envelope and leaves, which
// is a refusal either way.
//
// Executed, never reasoned about: a shell guard's verdict lives in the
// shell's own pattern matching, and `sh -n` would only prove it parses.
func runShellGuard(t *testing.T, guard, value string) bool {
	t.Helper()
	script := "err_envelope() { exit 1; }\n" + guard + "\nexit 0\n"
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "DS_AGENT="+value)
	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("the guard could not be executed: %v", err)
	}
	return false
}
