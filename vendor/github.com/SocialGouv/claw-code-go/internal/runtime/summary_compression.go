package runtime

import (
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/strutil"
	"sort"
	"strings"
	"unicode/utf8"
)

// Default budget constants matching Rust's summary_compression.rs.
const (
	defaultMaxChars     = 1200
	defaultMaxLines     = 24
	defaultMaxLineChars = 160
)

// SummaryCompressionBudget controls how aggressively a compaction summary is
// compressed. Zero values for any field cause the compressor to return empty
// output (matching Rust).
type SummaryCompressionBudget struct {
	MaxChars     int
	MaxLines     int
	MaxLineChars int
}

// DefaultBudget returns the default compression budget (1200 chars, 24 lines,
// 160 chars/line).
func DefaultBudget() SummaryCompressionBudget {
	return SummaryCompressionBudget{
		MaxChars:     defaultMaxChars,
		MaxLines:     defaultMaxLines,
		MaxLineChars: defaultMaxLineChars,
	}
}

// SummaryCompressionResult holds metrics produced by CompressSummary.
type SummaryCompressionResult struct {
	Summary               string
	OriginalChars         int
	CompressedChars       int
	OriginalLines         int
	CompressedLines       int
	RemovedDuplicateLines int
	OmittedLines          int
	Truncated             bool
}

// CompressSummary compresses a compaction summary text according to the given
// budget. It normalises whitespace, removes duplicate lines, prioritises
// important lines (headers, core details, bullets), and truncates to fit.
func CompressSummary(summary string, budget SummaryCompressionBudget) SummaryCompressionResult {
	originalChars := utf8.RuneCountInString(summary)
	originalLines := countLines(summary)

	normalized := normalizeLines(summary, budget.MaxLineChars)

	if len(normalized.lines) == 0 || budget.MaxChars == 0 || budget.MaxLines == 0 {
		return SummaryCompressionResult{
			Summary:               "",
			OriginalChars:         originalChars,
			CompressedChars:       0,
			OriginalLines:         originalLines,
			CompressedLines:       0,
			RemovedDuplicateLines: normalized.removedDuplicateLines,
			OmittedLines:          len(normalized.lines),
			Truncated:             originalChars > 0,
		}
	}

	selected := selectLineIndexes(normalized.lines, budget)
	var compressedLines []string
	for _, idx := range selected {
		compressedLines = append(compressedLines, normalized.lines[idx])
	}
	if len(compressedLines) == 0 {
		compressedLines = append(compressedLines, truncateLine(normalized.lines[0], budget.MaxChars))
	}

	omittedLines := max(0, len(normalized.lines)-len(compressedLines))

	if omittedLines > 0 {
		notice := omissionNotice(omittedLines)
		pushLineWithBudget(&compressedLines, notice, budget)
	}

	compressedSummary := strings.Join(compressedLines, "\n")

	return SummaryCompressionResult{
		Summary:               compressedSummary,
		OriginalChars:         originalChars,
		CompressedChars:       utf8.RuneCountInString(compressedSummary),
		OriginalLines:         originalLines,
		CompressedLines:       len(compressedLines),
		RemovedDuplicateLines: normalized.removedDuplicateLines,
		OmittedLines:          omittedLines,
		Truncated:             compressedSummary != strings.TrimSpace(summary),
	}
}

// CompressSummaryText is a convenience wrapper that returns only the compressed
// summary string using the default budget.
func CompressSummaryText(summary string) string {
	return CompressSummary(summary, DefaultBudget()).Summary
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

type normalizedSummary struct {
	lines                 []string
	removedDuplicateLines int
}

func normalizeLines(summary string, maxLineChars int) normalizedSummary {
	seen := map[string]bool{}
	var lines []string
	removedDups := 0

	for _, rawLine := range splitLines(summary) {
		normalized := collapseInlineWhitespace(rawLine)
		if normalized == "" {
			continue
		}

		truncated := truncateLine(normalized, maxLineChars)
		key := dedupeKey(truncated)
		if seen[key] {
			removedDups++
			continue
		}
		seen[key] = true
		lines = append(lines, truncated)
	}

	return normalizedSummary{lines: lines, removedDuplicateLines: removedDups}
}

func selectLineIndexes(lines []string, budget SummaryCompressionBudget) []int {
	var selected []int

	isSelected := func(idx int) bool {
		for _, s := range selected {
			if s == idx {
				return true
			}
		}
		return false
	}

	for priority := 0; priority <= 3; priority++ {
		for idx, line := range lines {
			if isSelected(idx) || linePriority(line) != priority {
				continue
			}

			// Build sorted candidate list (current selection + new index).
			candidate := make([]int, len(selected)+1)
			copy(candidate, selected)
			candidate[len(selected)] = idx
			sort.Ints(candidate)

			candidateLines := make([]string, len(candidate))
			for i, ci := range candidate {
				candidateLines[i] = lines[ci]
			}

			if len(candidateLines) > budget.MaxLines || joinedCharCount(candidateLines) > budget.MaxChars {
				continue
			}

			selected = candidate
		}
	}

	return selected
}

func pushLineWithBudget(lines *[]string, line string, budget SummaryCompressionBudget) {
	candidate := make([]string, len(*lines)+1)
	copy(candidate, *lines)
	candidate[len(*lines)] = line

	if len(candidate) <= budget.MaxLines && joinedCharCount(candidate) <= budget.MaxChars {
		*lines = candidate
	}
}

func joinedCharCount(lines []string) int {
	total := 0
	for _, line := range lines {
		total += utf8.RuneCountInString(line)
	}
	if len(lines) > 1 {
		total += len(lines) - 1 // newline separators
	}
	return total
}

func linePriority(line string) int {
	if line == "Summary:" || line == "Conversation summary:" || isCoreDetail(line) {
		return 0
	}
	if isSectionHeader(line) {
		return 1
	}
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "  - ") {
		return 2
	}
	return 3
}

// coreDetailPrefixes is the set of known important line prefixes.
var coreDetailPrefixes = []string{
	"- Scope:",
	"- Current work:",
	"- Pending work:",
	"- Key files referenced:",
	"- Tools mentioned:",
	"- Recent user requests:",
	"- Previously compacted context:",
	"- Newly compacted context:",
}

func isCoreDetail(line string) bool {
	for _, prefix := range coreDetailPrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func isSectionHeader(line string) bool {
	return strings.HasSuffix(line, ":")
}

func omissionNotice(omittedLines int) string {
	return fmt.Sprintf("- … %d additional line(s) omitted.", omittedLines)
}

func collapseInlineWhitespace(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

func truncateLine(line string, maxChars int) string {
	if maxChars == 0 || utf8.RuneCountInString(line) <= maxChars {
		return line
	}
	if maxChars == 1 {
		return "…"
	}

	runes := []rune(line)
	return string(runes[:maxChars-1]) + "…"
}

func dedupeKey(line string) string {
	return strutil.ASCIIToLower(line)
}

// splitLines splits a string into lines, handling \r\n, \r, and \n line
// endings. This matches Rust's str::lines() semantics.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

// countLines counts lines using the same semantics as Rust's str::lines()
// (handles \r\n, \r, and \n; trailing empty line is not counted).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	// Normalize to \n to match splitLines semantics.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	n := strings.Count(s, "\n")
	// Rust's .lines() doesn't count a trailing empty split.
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
