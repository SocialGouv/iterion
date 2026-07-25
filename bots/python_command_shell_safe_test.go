package bots

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// commandBackticks extracts each `command: ` + "`...`" value from a .bot
// source. The value is backtick-delimited, so its content never contains a
// backtick — a plain scan is exact.
func commandBackticks(src string) []string {
	const marker = "command: `"
	var out []string
	for i := 0; ; {
		j := strings.Index(src[i:], marker)
		if j < 0 {
			break
		}
		start := i + j + len(marker)
		end := strings.Index(src[start:], "`")
		if end < 0 {
			break
		}
		out = append(out, src[start:start+end])
		i = start + end + 1
	}
	return out
}

var tmplRef = regexp.MustCompile(`\{\{[^}]*\}\}`)

// TestReviewPRPublishCommandsRunCleanly guards a bug class that has bitten the
// review-pr publish path twice: a `python3 -c "<body>"` tool command whose BODY
// contains a shell-significant character (a bare double-quote, an unescaped
// backtick) that ends the double-quoted shell argument early and SILENTLY
// truncates the python — exit 0, empty stdout, no work done. `iterion validate`
// cannot see it (the DSL treats the command as opaque). This executes each
// python-`-c` tool command with its `{{…}}` refs blanked, and asserts the
// script still runs to a NON-EMPTY output — a truncated body prints nothing.
func TestReviewPRPublishCommandsRunCleanly(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	data, err := os.ReadFile("review-pr/main.bot")
	if err != nil {
		t.Fatalf("read review-pr/main.bot: %v", err)
	}
	blocks := commandBackticks(string(data))
	ran := 0
	for _, blk := range blocks {
		if !strings.Contains(blk, `python3 -c "`) {
			continue
		}
		// Skip the diff_precheck command: it chdir's into a workspace and runs
		// git, so it can't execute standalone. Its shape (no findings/questions
		// interpolation) is not the fragile one; publish_review/publish_health
		// are. Detect them by a field they reference.
		if !strings.Contains(blk, "emit(") && !strings.Contains(blk, "degraded") {
			continue
		}
		// Render: blank every {{…}} ref (empty value). publish_review with an
		// empty PUB_URL emits its skip JSON; publish_health with blank counts
		// emits its OK banner — both NON-EMPTY when the body is intact.
		rendered := tmplRef.ReplaceAllString(blk, "")
		out, _ := exec.Command("sh", "-c", rendered).Output()
		if strings.TrimSpace(string(out)) == "" {
			t.Errorf("review-pr/main.bot: a python3 -c tool command produced EMPTY output when run — its body is truncated by a stray shell metacharacter (bare double-quote or backtick). Keep the python -c body free of unescaped \" and `. Command head: %q",
				firstLine(rendered))
		}
		ran++
	}
	if ran == 0 {
		t.Fatal("no publish python commands found to exercise — the detection heuristic is stale")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
