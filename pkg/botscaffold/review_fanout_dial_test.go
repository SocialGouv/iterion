package botscaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewFanoutRuleListsFollowTheDial: the fan-out's allow/deny rule
// lists go with its permission dial. Cleared, they are not written either
// — a file carrying rule lists no gate reads would be an inert declared
// gate; on, the lists and the mode are both there.
func TestReviewFanoutRuleListsFollowTheDial(t *testing.T) {
	render := func(permission string) string {
		t.Helper()
		spec := templateForShape(t, "review-fanout").Spec
		spec.Slug = "rf"
		spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
		spec.Permission = permission
		dir := filepath.Join(t.TempDir(), spec.Slug)
		if _, err := Scaffold(dir, spec); err != nil {
			t.Fatalf("Scaffold(permission=%q): %v", permission, err)
		}
		src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	on := render("deny")
	if !strings.Contains(on, "\n  permission: deny") || !strings.Contains(on, "\n  allow: [") || !strings.Contains(on, "\n  deny: [") {
		t.Fatalf("with the dial on, the rendered workflow lacks the mode or a list:\n%s", on)
	}
	off := render("")
	if strings.Contains(off, "\n  permission:") || strings.Contains(off, "\n  allow: [") || strings.Contains(off, "\n  deny: [") {
		t.Fatalf("with the dial cleared, the rendered workflow still carries a gate or its lists:\n%s", off)
	}
}
