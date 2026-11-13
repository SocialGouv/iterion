package runtime

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompressSummary_CollapsesWhitespaceAndDuplicates(t *testing.T) {
	summary := "Conversation summary:\n\n- Scope:   compact   earlier   messages.\n- Scope: compact earlier messages.\n- Current work: update runtime module.\n"

	result := CompressSummary(summary, DefaultBudget())

	if result.RemovedDuplicateLines != 1 {
		t.Errorf("expected 1 removed duplicate, got %d", result.RemovedDuplicateLines)
	}
	if !strings.Contains(result.Summary, "- Scope: compact earlier messages.") {
		t.Errorf("expected normalised Scope line in output, got:\n%s", result.Summary)
	}
	if strings.Contains(result.Summary, "  compact   earlier") {
		t.Errorf("multi-space run should have been collapsed, got:\n%s", result.Summary)
	}
}

func TestCompressSummary_KeepsCoreWhenBudgetTight(t *testing.T) {
	lines := []string{
		"Conversation summary:",
		"- Scope: 18 earlier messages compacted.",
		"- Current work: finish summary compression.",
		"- Key timeline:",
		"  - user: asked for a working implementation.",
		"  - assistant: inspected runtime compaction flow.",
		"  - tool: cargo check succeeded.",
	}
	summary := strings.Join(lines, "\n")

	result := CompressSummary(summary, SummaryCompressionBudget{
		MaxChars:     120,
		MaxLines:     3,
		MaxLineChars: 80,
	})

	if !strings.Contains(result.Summary, "Conversation summary:") {
		t.Errorf("expected header retained, got:\n%s", result.Summary)
	}
	if !strings.Contains(result.Summary, "- Scope: 18 earlier messages compacted.") {
		t.Errorf("expected Scope line retained, got:\n%s", result.Summary)
	}
	if !strings.Contains(result.Summary, "- Current work: finish summary compression.") {
		t.Errorf("expected Current work line retained, got:\n%s", result.Summary)
	}
	if result.OmittedLines <= 0 {
		t.Error("expected OmittedLines > 0")
	}
}

func TestCompressSummaryText_DefaultHelper(t *testing.T) {
	summary := "Summary:\n\nA short line."

	compressed := CompressSummaryText(summary)

	expected := "Summary:\nA short line."
	if compressed != expected {
		t.Errorf("expected %q, got %q", expected, compressed)
	}
}

func TestCompressSummary_EmptyInput(t *testing.T) {
	result := CompressSummary("", DefaultBudget())

	if result.Summary != "" {
		t.Errorf("expected empty summary, got %q", result.Summary)
	}
	if result.CompressedChars != 0 {
		t.Errorf("expected 0 compressed chars, got %d", result.CompressedChars)
	}
	if result.Truncated {
		t.Error("expected Truncated=false for empty input")
	}
}

func TestTruncateLine_Unicode(t *testing.T) {
	// Rune-aware truncation: "héllo wörld" has 11 runes.
	line := "héllo wörld"

	truncated := truncateLine(line, 6)

	// 5 runes + ellipsis = "héllo…"
	expected := "héllo…"
	if truncated != expected {
		t.Errorf("expected %q, got %q", expected, truncated)
	}
}

func TestCompressSummary_ZeroBudget(t *testing.T) {
	result := CompressSummary("Some text here", SummaryCompressionBudget{
		MaxChars:     0,
		MaxLines:     24,
		MaxLineChars: 160,
	})

	if result.Summary != "" {
		t.Errorf("expected empty summary for zero MaxChars, got %q", result.Summary)
	}
	if !result.Truncated {
		t.Error("expected Truncated=true for non-empty input with zero budget")
	}
}

func TestLinePriority(t *testing.T) {
	cases := []struct {
		line     string
		expected int
	}{
		{"Conversation summary:", 0},
		{"Summary:", 0},
		{"- Scope: 10 messages", 0},
		{"- Current work: something", 0},
		{"- Key timeline:", 1},
		{"Section heading:", 1},
		{"- a bullet point", 2},
		{"  - indented bullet", 2},
		{"arbitrary text line", 3},
	}

	for _, tc := range cases {
		got := linePriority(tc.line)
		if got != tc.expected {
			t.Errorf("linePriority(%q) = %d, want %d", tc.line, got, tc.expected)
		}
	}
}

func TestCompressSummary_TruncateLine_MaxCharsOne(t *testing.T) {
	result := truncateLine("hello", 1)
	if result != "…" {
		t.Errorf("expected '…' for maxChars=1, got %q", result)
	}
}

// --- Parity tests for Batch 6 fixes ---

func TestDedupeKey_ASCIIOnly(t *testing.T) {
	// ASCII letters: same key
	if dedupeKey("HELLO") != dedupeKey("hello") {
		t.Error("ASCII 'HELLO' and 'hello' should produce the same dedupeKey")
	}
	// Non-ASCII: NOT folded (Ü ≠ ü in ASCII-only lowering)
	if dedupeKey("Über") == dedupeKey("über") {
		t.Error("non-ASCII 'Über' and 'über' should produce DIFFERENT dedupeKeys (ASCII-only)")
	}
}

func TestSplitLines_CRLF(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"CRLF", "a\r\nb\r\nc", 3},
		{"CR only", "a\rb\rc", 3},
		{"LF only", "a\nb\nc", 3},
		{"mixed", "a\r\nb\rc\nd", 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLines(tt.input)
			if len(got) != tt.want {
				t.Errorf("splitLines(%q) produced %d lines, want %d", tt.input, len(got), tt.want)
			}
		})
	}
}

func TestNormalizeLines_CRLFHandling(t *testing.T) {
	summary := "line one\r\nline two\r\nline three"
	result := normalizeLines(summary, 160)
	if len(result.lines) != 3 {
		t.Errorf("expected 3 lines after CRLF normalization, got %d: %v", len(result.lines), result.lines)
	}
}

func TestCompressSummary_Integration_FitsDefaultBudget(t *testing.T) {
	// Build a summary that exceeds default budget to verify compression.
	var sb strings.Builder
	sb.WriteString("Conversation summary:\n")
	sb.WriteString("- Scope: 50 earlier messages compacted (user=20, assistant=25, tool=5).\n")
	sb.WriteString("- Tools mentioned: Read, Write, Bash, Grep.\n")
	sb.WriteString("- Current work: implementing summary compression.\n")
	sb.WriteString("- Key files referenced: internal/runtime/compact.go, internal/runtime/summary_compression.go.\n")
	sb.WriteString("- Key timeline:\n")
	for i := 0; i < 40; i++ {
		sb.WriteString(fmt.Sprintf("  - user: asked about implementation details for feature number %d with detail ", i))
		sb.WriteString(strings.Repeat("x", 80))
		sb.WriteString(".\n")
	}

	result := CompressSummary(sb.String(), DefaultBudget())

	if result.CompressedChars > defaultMaxChars {
		t.Errorf("compressed chars %d exceeds budget %d", result.CompressedChars, defaultMaxChars)
	}
	if result.CompressedLines > defaultMaxLines {
		t.Errorf("compressed lines %d exceeds budget %d", result.CompressedLines, defaultMaxLines)
	}
	if result.OmittedLines == 0 {
		t.Error("expected some lines to be omitted")
	}
}
