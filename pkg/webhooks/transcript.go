package webhooks

import (
	"strings"
	"unicode/utf8"
)

// transcriptSep separates rendered thread entries in a conversation
// transcript handed to a converse bot.
const transcriptSep = "\n\n---\n\n"

// CapTranscript joins rendered thread entries (chronological) into the
// transcript a converse bot receives, bounded by maxChars: the FIRST entry
// (the thread anchor — typically the review comment the operator replied to)
// is always kept, then the most recent entries fill the remaining budget,
// with an omission marker replacing the middle. maxChars <= 0 means no cap.
// Shared by the GitLab note lane and the GitHub review-thread lane so both
// transcripts obey one budget policy.
func CapTranscript(rendered []string, maxChars int) string {
	if len(rendered) == 0 {
		return ""
	}
	full := strings.Join(rendered, transcriptSep)
	if maxChars <= 0 || len(full) <= maxChars {
		return full
	}
	// Over budget: anchor + newest entries, omission marker in between.
	const omitted = "[… earlier notes omitted …]"
	anchor := rendered[0]
	budget := maxChars - len(anchor) - len(omitted) - 2*len(transcriptSep)
	var tail []string
	for i := len(rendered) - 1; i >= 1; i-- {
		need := len(rendered[i]) + len(transcriptSep)
		if need > budget {
			break
		}
		budget -= need
		tail = append([]string{rendered[i]}, tail...)
	}
	parts := []string{anchor}
	if len(tail) < len(rendered)-1 {
		parts = append(parts, omitted)
	}
	parts = append(parts, tail...)
	out := strings.Join(parts, transcriptSep)
	if len(out) > maxChars {
		// The anchor alone overflows the budget — hard-truncate on a rune
		// boundary so the transcript stays valid UTF-8.
		cut := maxChars
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "\n[… truncated …]"
	}
	return out
}
