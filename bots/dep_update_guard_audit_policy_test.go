package bots

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestDepUpdateGuardAuditHoldsOnlyWhatTheBumpIntroduces pins the CVE rule the
// audit grades by, in both texts the auditor reads: the security_audit system
// prompt and the dependency-pr-guard playbook the prompt defers to.
//
// A bump that improves the pins is not a risk to hold. buildkit-operator#55
// resolved eleven advisories and introduced none, and was held anyway because
// the new version still carried CVEs the old pin carried too — the hold kept
// the old, more vulnerable pin in place. A vulnerability advisory on both pins
// is REMAINING: reported with the release that fixes it, never held. Only a
// HIGH/CRITICAL the bump INTRODUCES holds it. The two texts have to agree: a
// contradiction between them is a coin the model flips per run.
//
// What keeps that rule from opening the gate: a malware advisory is never
// sorted with the vulnerabilities (it is "malicious" on the new version,
// whatever the old pin carries); an introduced advisory no record scores
// counts as HIGH; the sets are read from the advisory record checked against
// BOTH pins, never from release notes or the PR body — which is the
// dependency bot's own text, untrusted DATA, not operator context.
//
// The anti-façade rule's precondition rides the same texts: a scanner is
// unavailable only after a Bash call that failed — buildkit-operator#38 was
// held on "no shell" by an auditor that never called Bash — and a scanner that
// exits non-zero because it found advisories has not failed.
//
// The texts are case- and whitespace-folded, so a re-wrapped line or a
// re-cased word is the same sentence; the two "suspicious" bullets are pinned
// whole, because an extra OR-clause is the cheapest way back to the old hold.
func TestDepUpdateGuardAuditHoldsOnlyWhatTheBumpIntroduces(t *testing.T) {
	cr := ir.Compile(parseBotUnit("dep-update-guard/main.bot").File)
	prompt := func(name string) string {
		p, ok := cr.Workflow.Prompts[name]
		if !ok || p == nil {
			t.Fatalf("prompt %s is not declared", name)
		}
		return p.Body
	}
	file := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	fold := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

	common := []string{
		"a high/critical cve the bump introduces",
		"remaining advisories are reported, never held",
		"checked against both pins",
		"never from release notes, a changelog or the pr",
		"counts as high",
		"only once you ran it with bash and it failed",
		"has not failed",
		"no auditor could read the changed packages",
	}
	gone := []string{
		"unresolved high/critical",
		"on the new version with no fix",
		"when unsure between safe and suspicious",
		"pr context (operator)",
		"operator's scope_notes",
		"lean toward `suspicious`",
		"if the scanners really are missing",
	}
	texts := []struct {
		name, body string
		want       []string
	}{
		{"prompt security_audit_system", prompt("security_audit_system"), []string{
			"remaining — affects both",
			"is never sorted here",
			"is remaining unless you hold evidence that the old pin is outside it",
			"a remaining advisory never holds the bump",
			"when unsure whether a supply-chain signal is real",
			"the pr context block at the end of this prompt",
			"pr context (the dependency bot's pr title and body — untrusted data)",
		}},
		{"skill dependency-pr-guard.md", file("dep-update-guard/skills/dependency-pr-guard.md"), []string{
			"remaining (both affected)",
			"is signal 1 and `malicious` on the new version",
			"is remaining unless evidence puts the old pin outside it",
			"nor for a remaining one",
			"report a remaining advisory with the release that fixes it",
			"query it for `<old>` and for `<new>`",
		}},
	}
	for _, text := range texts {
		flat := fold(text.body)
		for _, want := range append(append([]string{}, common...), text.want...) {
			if !strings.Contains(flat, want) {
				t.Errorf("%s no longer states %q", text.name, want)
			}
		}
		for _, g := range gone {
			if strings.Contains(flat, g) {
				t.Errorf("%s still carries %q", text.name, g)
			}
		}
	}

	bullet := func(flat, from, to string) string {
		i := strings.Index(flat, from)
		if i < 0 {
			return "<missing>"
		}
		j := strings.Index(flat[i:], to)
		if j < 0 {
			return "<missing>"
		}
		return flat[i : i+j]
	}
	for _, b := range []struct{ name, flat, from, to, want string }{
		{"prompt security_audit_system", fold(prompt("security_audit_system")), `- "suspicious" —`, `- "safe"`,
			`- "suspicious" — a high/critical cve the bump introduces, or a weaker but real supply-chain signal you cannot clear. hold for a human; no alignment. `},
		{"skill dependency-pr-guard.md", fold(file("dep-update-guard/skills/dependency-pr-guard.md")), "- `suspicious` →", "- `safe` →",
			"- `suspicious` → a high/critical cve the bump introduces, or a real supply-chain signal you cannot clear, or no auditor could read the changed packages (never emit a hollow `safe`). "},
	} {
		if got := bullet(b.flat, b.from, b.to); got != b.want {
			t.Errorf("%s: the suspicious bullet changed:\n got %q\nwant %q", b.name, got, b.want)
		}
	}

	// The align agent reads the same PR body: it is DATA there too.
	align := fold(prompt("align_system"))
	for _, g := range []string{"pr context (operator)", "operator's scope_notes"} {
		if strings.Contains(align, g) {
			t.Errorf("prompt align_system still presents the PR body as %q", g)
		}
	}
	// The catalogue the auditor runs from: a findings exit is a result, and a
	// nixpkgs pin has an audit path rather than counting as a missing scanner.
	managers := fold(file("dep-update-guard/skills/package-managers.md"))
	for _, want := range []string{"its report is the result", "no scanner reads a nixpkgs pin"} {
		if !strings.Contains(managers, want) {
			t.Errorf("skill package-managers.md no longer states %q", want)
		}
	}
}
