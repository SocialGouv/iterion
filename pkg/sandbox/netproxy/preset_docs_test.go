package netproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docs/sandbox.md prints the iterion-default preset verbatim: operators
// and data-protection reviewers read the page, not preset.go, to learn
// which destinations an allowlist run may reach. The page and the code
// name the same rules, each as often: a rule the page does not list, a
// host the page lists but the preset does not carry, and a rule listed a
// different number of times all redden. The page groups rules by family,
// so their order is not compared.
func TestPresetDocsNameTheSameRules(t *testing.T) {
	want := iterionDefaultRules()
	got := presetRulesOnThePage(t)
	if len(got) != len(want) {
		t.Errorf("docs/sandbox.md's iterion-default block lists %d rules, the preset carries %d — a rule listed twice, or missing", len(got), len(want))
	}
	for _, rule := range want {
		if !containsRule(got, rule) {
			t.Errorf("docs/sandbox.md's iterion-default block does not list %q — the preset admits a host the page does not say", rule)
		}
	}
	for _, rule := range got {
		if !containsRule(want, rule) {
			t.Errorf("docs/sandbox.md's iterion-default block lists %q, which the preset does not carry", rule)
		}
	}
}

func containsRule(rules []string, rule string) bool {
	for _, r := range rules {
		if r == rule {
			return true
		}
	}
	return false
}

// presetRulesOnThePage reads the text fence that follows the preset's
// introduction in docs/sandbox.md: one or more comma-separated rules per
// line, a `#` opening a comment, a line that is only a comment skipped.
func presetRulesOnThePage(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "sandbox.md"))
	if err != nil {
		t.Fatalf("read docs/sandbox.md: %v", err)
	}
	page := string(raw)
	const intro = "Its rules, verbatim from"
	at := strings.Index(page, intro)
	if at < 0 {
		t.Fatalf("docs/sandbox.md no longer introduces the preset with %q", intro)
	}
	rest := page[at:]
	open := strings.Index(rest, "```text\n")
	if open < 0 {
		t.Fatalf("docs/sandbox.md: no ```text fence after %q", intro)
	}
	rest = rest[open+len("```text\n"):]
	closeAt := strings.Index(rest, "```")
	if closeAt < 0 {
		t.Fatalf("docs/sandbox.md: the preset fence never closes")
	}
	var rules []string
	for _, line := range strings.Split(rest[:closeAt], "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, field := range strings.Split(line, ",") {
			if field = strings.TrimSpace(field); field != "" {
				rules = append(rules, field)
			}
		}
	}
	if len(rules) == 0 {
		t.Fatalf("docs/sandbox.md: the preset fence lists no rule")
	}
	return rules
}
