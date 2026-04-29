package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

// Compaction continuation constants (matching Rust compact.rs).
const (
	compactContinuationPreamble    = "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\n"
	compactRecentMessagesNote      = "Recent messages are preserved verbatim."
	compactDirectResumeInstruction = "Continue the conversation from where it left off without asking the user any further questions. Resume directly — do not acknowledge the summary, do not recap what was happening, and do not preface with continuation text."
)

// analysisTagRe strips <analysis>...</analysis> tags from summaries.
var analysisTagRe = regexp.MustCompile(`(?s)<analysis>.*?</analysis>`)

// summaryTagRe extracts content from <summary>...</summary> tags.
var summaryTagRe = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)

const (
	// DefaultCompactionThreshold triggers compaction when input tokens reach
	// this fraction of MaxTokens (e.g., 0.75 = 75%).
	DefaultCompactionThreshold = 0.75

	// DefaultCompactionKeepRecent is the number of recent messages retained
	// verbatim after compaction.
	DefaultCompactionKeepRecent = 10

	// charsPerToken is the approximate character-to-token ratio used for estimation.
	charsPerToken = 4
)

// CompactionState tracks token usage and compaction history across turns.
type CompactionState struct {
	LastInputTokens   int // input tokens from the most recently completed turn
	TotalInputTokens  int // cumulative input tokens across all turns
	TotalOutputTokens int // cumulative output tokens across all turns
	CompactionCount   int // number of times the session has been compacted
}

// EstimateTokens roughly estimates the number of tokens in a slice of messages
// using a simple chars-per-token heuristic. Matches Rust's
// estimate_message_tokens() with explicit ToolUse and ToolResult arms:
//   - Text: text.len() / 4 + 1
//   - ToolUse: (name.len() + input_json.len()) / 4 + 1
//   - ToolResult: (tool_name.len() + output.len()) / 4 + 1
func EstimateTokens(messages []api.Message) int {
	var total int
	for _, msg := range messages {
		for _, cb := range msg.Content {
			total += estimateBlockTokens(&cb)
		}
	}
	return total
}

// estimateBlockTokens estimates tokens for a single content block.
func estimateBlockTokens(cb *api.ContentBlock) int {
	switch cb.Type {
	case "tool_use":
		// Rust: (name.len() + input.len()) / 4 + 1
		inputLen := 0
		if cb.Input != nil {
			// Serialize input map to approximate JSON length
			data, err := json.Marshal(cb.Input)
			if err == nil {
				inputLen = len(data)
			}
		}
		return (len(cb.Name)+inputLen)/charsPerToken + 1
	case "tool_result":
		// Rust: (tool_name.len() + output.len()) / 4 + 1
		// tool_name is not stored directly; use ToolUseID as proxy.
		// Output is the nested text content.
		outputLen := 0
		for _, inner := range cb.Content {
			outputLen += len(inner.Text)
		}
		return (len(cb.ToolUseID)+outputLen)/charsPerToken + 1
	default:
		// Text blocks and others
		return len(cb.Text)/charsPerToken + 1
	}
}

// CountRealUserTurns counts messages with role "user" that are not injected.
// This is used for turn accounting that should exclude programmatically-injected
// messages (e.g., via InjectPrompt).
func CountRealUserTurns(messages []api.Message) int {
	count := 0
	for _, msg := range messages {
		if msg.Role == "user" && !msg.IsInjected {
			count++
		}
	}
	return count
}

// minRealTurnsForCompaction is the minimum number of real (non-injected) user
// turns required before compaction is considered. This prevents injected
// system messages from inflating the raw message count and triggering
// premature compaction.
const minRealTurnsForCompaction = 2

// ShouldCompact returns true when the session should be compacted.
// It uses the actual API-reported input token count when available (> 0),
// falling back to EstimateTokens. Additionally, it requires at least
// minRealTurnsForCompaction real (non-injected) user turns, so that
// sessions dominated by injected messages don't trigger premature compaction.
func ShouldCompact(inputTokens int, messages []api.Message, cfg *Config) bool {
	if !cfg.CompactionEnabled {
		return false
	}
	// Don't compact if there aren't enough real user turns yet.
	if CountRealUserTurns(messages) < minRealTurnsForCompaction {
		return false
	}
	if inputTokens <= 0 {
		inputTokens = EstimateTokens(messages)
	}
	threshold := int(float64(cfg.MaxTokens) * cfg.CompactionThreshold)
	return inputTokens >= threshold
}

// collectStreamText drains a StreamEvent channel and returns the concatenated
// text response, or an error if the stream encountered one.
func collectStreamText(ch <-chan api.StreamEvent) (string, error) {
	var sb strings.Builder
	for event := range ch {
		switch event.Type {
		case api.EventError:
			return "", fmt.Errorf("stream error: %s", event.ErrorMessage)
		case api.EventContentBlockDelta:
			if event.Delta.Type == "text_delta" {
				sb.WriteString(event.Delta.Text)
			}
		}
	}
	return sb.String(), nil
}

// buildTranscript constructs a plain-text transcript of the messages suitable
// for submission to the summarization model.
func buildTranscript(messages []api.Message) string {
	var sb strings.Builder
	sb.WriteString("Please provide a concise but thorough summary of the following conversation. ")
	sb.WriteString("Preserve all important technical details: file paths modified, commands run, ")
	sb.WriteString("decisions made, errors encountered, and the current state of any ongoing work. ")
	sb.WriteString("The summary will replace the conversation history and must be self-contained.\n\n")
	sb.WriteString("---CONVERSATION---\n")

	for _, msg := range messages {
		fmt.Fprintf(&sb, "\n[%s]:\n", strings.ToUpper(msg.Role))
		for _, cb := range msg.Content {
			switch cb.Type {
			case "text":
				if len(cb.Text) > 1000 {
					sb.WriteString(cb.Text[:1000])
					sb.WriteString("... [truncated]\n")
				} else {
					sb.WriteString(cb.Text)
					sb.WriteString("\n")
				}
			case "tool_use":
				fmt.Fprintf(&sb, "[Tool call: %s]\n", cb.Name)
			case "tool_result":
				for _, inner := range cb.Content {
					if len(inner.Text) > 300 {
						fmt.Fprintf(&sb, "[Tool result: %s... (truncated)]\n", inner.Text[:300])
					} else {
						fmt.Fprintf(&sb, "[Tool result: %s]\n", inner.Text)
					}
				}
			}
		}
	}
	sb.WriteString("\n---END CONVERSATION---\n")
	return sb.String()
}

// CompactSession summarizes the session's message history by calling the model,
// stores the summary in the session, and trims the message list to the most
// recent cfg.CompactionKeepRecent messages. Returns the summary text.
func CompactSession(ctx context.Context, client api.APIClient, cfg *Config, session *Session) (string, error) {
	if len(session.Messages) == 0 {
		return "", nil
	}

	transcript := buildTranscript(session.Messages)

	req := api.CreateMessageRequest{
		Model:     cfg.Model,
		MaxTokens: 2048,
		Messages: []api.Message{
			{
				Role: "user",
				Content: []api.ContentBlock{
					{Type: "text", Text: transcript},
				},
			},
		},
		Stream: true,
	}

	ch, err := client.StreamResponse(ctx, req)
	if err != nil {
		return "", fmt.Errorf("compact: stream response: %w", err)
	}

	summary, err := collectStreamText(ch)
	if err != nil {
		return "", fmt.Errorf("compact: collect stream: %w", err)
	}

	// Merge with previous summary if exists (must happen BEFORE building
	// the continuation message so the injected context includes all prior
	// compacted material — matching Rust's compact_session).
	mergedSummary := summary
	if session.CompactionSummary != "" {
		mergedSummary = MergeCompactSummaries(session.CompactionSummary, summary)
	}

	// Retain the most recent N messages verbatim.
	keepCount := cfg.CompactionKeepRecent
	if keepCount > len(session.Messages) {
		keepCount = len(session.Messages)
	}
	recent := make([]api.Message, keepCount)
	copy(recent, session.Messages[len(session.Messages)-keepCount:])

	// Use the merged summary for the continuation message so that prior
	// compacted context is preserved (GetContinuationMessage formats internally).
	continuationMsg := GetContinuationMessage(mergedSummary, true, keepCount > 0)

	// Build new message list: continuation + recent.
	newMessages := make([]api.Message, 0, 1+len(recent))
	newMessages = append(newMessages, continuationMsg)
	newMessages = append(newMessages, recent...)

	session.CompactionSummary = CompressSummaryText(mergedSummary)
	session.CompactionCount++
	session.Messages = newMessages

	return summary, nil
}

// FormatCompactSummary cleans and formats a compaction summary for injection
// into the system prompt. Strips <analysis> tags and extracts <summary> content.
func FormatCompactSummary(summary string) string {
	// Strip <analysis>...</analysis> tags.
	cleaned := analysisTagRe.ReplaceAllString(summary, "")

	// Replace <summary>...</summary> tags with "Summary:\n" prefix.
	if matches := summaryTagRe.FindStringSubmatch(cleaned); len(matches) > 1 {
		cleaned = summaryTagRe.ReplaceAllString(cleaned, "Summary:\n"+strings.TrimSpace(matches[1]))
	}

	// Collapse multiple blank lines.
	cleaned = collapseBlankLines(cleaned)

	return strings.TrimSpace(cleaned)
}

// collapseBlankLines replaces runs of multiple blank lines with a single blank line.
var multipleBlankLinesRe = regexp.MustCompile(`\n{3,}`)

func collapseBlankLines(s string) string {
	return multipleBlankLinesRe.ReplaceAllString(s, "\n\n")
}

// GetContinuationMessage creates a synthetic system message that announces the
// compaction event, suitable for prepending to the retained recent messages.
//
// The summary is normalized via FormatCompactSummary before injection (matching
// Rust's get_compact_continuation_message which calls format_compact_summary
// internally).
//
// When suppressFollowUp is true, the message includes an instruction to not
// acknowledge the summary or ask follow-up questions.
// When recentPreserved is true, a note about preserved recent messages is added.
func GetContinuationMessage(summary string, suppressFollowUp, recentPreserved bool) api.Message {
	var sb strings.Builder
	sb.WriteString(compactContinuationPreamble)
	sb.WriteString(FormatCompactSummary(summary))

	if recentPreserved {
		sb.WriteString("\n\n")
		sb.WriteString(compactRecentMessagesNote)
	}

	if suppressFollowUp {
		sb.WriteString("\n")
		sb.WriteString(compactDirectResumeInstruction)
	}

	return api.Message{
		Role: "system",
		Content: []api.ContentBlock{
			{Type: "text", Text: sb.String()},
		},
	}
}

// MergeCompactSummaries merges two compaction summaries into one with structured
// sections. The current summary is formatted via FormatCompactSummary before
// extracting highlights and timeline, preventing raw <analysis> tags from
// leaking into the merged output (matching Rust merge_compact_summaries).
func MergeCompactSummaries(previous, current string) string {
	if previous == "" {
		return current
	}
	if current == "" {
		return previous
	}

	prevHighlights := extractSummaryHighlights(previous)
	newFormatted := FormatCompactSummary(current)
	newHighlights := extractSummaryHighlights(newFormatted)
	newTimeline := extractSummaryTimeline(newFormatted)

	var sb strings.Builder
	sb.WriteString("<summary>\n")
	sb.WriteString("Conversation summary:\n")

	if len(prevHighlights) > 0 {
		sb.WriteString("- Previously compacted context:\n")
		for _, h := range prevHighlights {
			sb.WriteString("  ")
			sb.WriteString(h)
			sb.WriteString("\n")
		}
	}

	if len(newHighlights) > 0 {
		sb.WriteString("- Newly compacted context:\n")
		for _, h := range newHighlights {
			sb.WriteString("  ")
			sb.WriteString(h)
			sb.WriteString("\n")
		}
	}

	if len(newTimeline) > 0 {
		sb.WriteString("- Key timeline:\n")
		for _, h := range newTimeline {
			sb.WriteString("  ")
			sb.WriteString(h)
			sb.WriteString("\n")
		}
	}

	sb.WriteString("</summary>")
	return sb.String()
}

// extractSummaryHighlights extracts non-timeline content lines from a summary.
// The summary is formatted via FormatCompactSummary first (matching Rust which
// calls format_compact_summary inside extract_summary_highlights).
func extractSummaryHighlights(summary string) []string {
	var highlights []string
	inTimeline := false
	formatted := FormatCompactSummary(summary)
	for _, line := range strings.Split(formatted, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "" || trimmed == "Summary:" || trimmed == "Conversation summary:" {
			continue
		}
		if trimmed == "- Key timeline:" {
			inTimeline = true
			continue
		}
		if inTimeline {
			continue
		}
		highlights = append(highlights, trimmed)
	}
	return highlights
}

// extractSummaryTimeline extracts timeline lines from a summary.
// The summary is formatted via FormatCompactSummary first (matching Rust which
// calls format_compact_summary inside extract_summary_timeline).
func extractSummaryTimeline(summary string) []string {
	var timeline []string
	inTimeline := false
	formatted := FormatCompactSummary(summary)
	for _, line := range strings.Split(formatted, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "- Key timeline:" {
			inTimeline = true
			continue
		}
		if !inTimeline {
			continue
		}
		if trimmed == "" {
			break
		}
		timeline = append(timeline, trimmed)
	}
	return timeline
}

// ---------------------------------------------------------------------------
// Pure-function compaction pipeline (no LLM calls)
// ---------------------------------------------------------------------------

// CompactionConfig controls pure-function compaction behavior.
type CompactionConfig struct {
	PreserveRecentMessages int // default 4
	MaxEstimatedTokens     int // default 10000
}

// DefaultCompactionConfig returns the default compaction configuration.
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{PreserveRecentMessages: 4, MaxEstimatedTokens: 10000}
}

// CompactionResult holds the output of a pure-function compaction.
type CompactionResult struct {
	Summary             string
	FormattedSummary    string
	CompactedMessages   []api.Message // new message list after compaction
	RemovedMessageCount int
}

// ShouldCompactPure checks if messages should be compacted based on config.
// It looks at compactable messages (after any existing compacted prefix)
// and checks: len > preserve_recent AND estimated_tokens >= max_estimated_tokens.
func ShouldCompactPure(messages []api.Message, cfg CompactionConfig) bool {
	compactable := compactableMessages(messages)
	if len(compactable) <= cfg.PreserveRecentMessages {
		return false
	}
	return EstimateTokens(compactable) >= cfg.MaxEstimatedTokens
}

// compactableMessages returns the subset of messages eligible for compaction,
// skipping any leading system message that is an existing compacted summary.
func compactableMessages(messages []api.Message) []api.Message {
	if len(messages) == 0 {
		return messages
	}
	if extractExistingCompactedSummary(messages[0]) != "" {
		return messages[1:]
	}
	return messages
}

// CompactSessionPure performs pure-function compaction without LLM.
// Returns nil if compaction is not needed.
func CompactSessionPure(messages []api.Message, cfg CompactionConfig) *CompactionResult {
	if !ShouldCompactPure(messages, cfg) {
		return nil
	}

	compactable := compactableMessages(messages)
	keepCount := cfg.PreserveRecentMessages
	if keepCount > len(compactable) {
		keepCount = len(compactable)
	}

	toSummarize := compactable[:len(compactable)-keepCount]
	recent := compactable[len(compactable)-keepCount:]

	summary := SummarizeMessages(toSummarize)

	// Merge with any existing compacted summary from the first message.
	existingSummary := ""
	if len(messages) > 0 {
		existingSummary = extractExistingCompactedSummary(messages[0])
	}
	mergedSummary := summary
	if existingSummary != "" {
		mergedSummary = MergeCompactSummaries(existingSummary, summary)
	}

	formatted := FormatCompactSummary(mergedSummary)
	continuationMsg := GetContinuationMessage(mergedSummary, true, keepCount > 0)

	newMessages := make([]api.Message, 0, 1+len(recent))
	newMessages = append(newMessages, continuationMsg)
	newMessages = append(newMessages, recent...)

	removedCount := len(messages) - len(recent)

	compressedSummary := CompressSummaryText(mergedSummary)

	return &CompactionResult{
		Summary:             compressedSummary,
		FormattedSummary:    formatted,
		CompactedMessages:   newMessages,
		RemovedMessageCount: removedCount,
	}
}

// SummarizeMessages produces a <summary>...</summary> structured summary
// of the given messages using pure heuristics (no LLM call).
func SummarizeMessages(messages []api.Message) string {
	// 1. Count messages by role.
	roleCounts := map[string]int{}
	for _, msg := range messages {
		roleCounts[msg.Role]++
	}

	// 2. Extract unique tool names.
	toolSet := map[string]bool{}
	for _, msg := range messages {
		for _, cb := range msg.Content {
			if cb.Type == "tool_use" && cb.Name != "" {
				toolSet[cb.Name] = true
			}
			if cb.Type == "tool_result" {
				for _, inner := range cb.Content {
					if inner.Type == "tool_use" && inner.Name != "" {
						toolSet[inner.Name] = true
					}
				}
			}
		}
	}
	var toolNames []string
	for name := range toolSet {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)

	// 3. Build summary.
	var sb strings.Builder
	sb.WriteString("<summary>\n")
	sb.WriteString("Conversation summary:\n")

	// Scope line.
	sb.WriteString(fmt.Sprintf("- Scope: %d earlier messages compacted (user=%d, assistant=%d, tool=%d).\n",
		len(messages), roleCounts["user"], roleCounts["assistant"], roleCounts["tool"]))

	// Tools mentioned.
	if len(toolNames) > 0 {
		sb.WriteString("- Tools mentioned: ")
		sb.WriteString(strings.Join(toolNames, ", "))
		sb.WriteString(".\n")
	}

	// 4. Recent user requests.
	recentRequests := collectRecentRoleSummaries(messages, "user", 3)
	if len(recentRequests) > 0 {
		sb.WriteString("- Recent user requests:\n")
		for _, r := range recentRequests {
			sb.WriteString("  - ")
			sb.WriteString(r)
			sb.WriteString("\n")
		}
	}

	// 5. Pending work.
	pending := inferPendingWork(messages)
	if len(pending) > 0 {
		sb.WriteString("- Pending work:\n")
		for _, p := range pending {
			sb.WriteString("  - ")
			sb.WriteString(p)
			sb.WriteString("\n")
		}
	}

	// 6. Key files.
	keyFiles := collectKeyFiles(messages)
	if len(keyFiles) > 0 {
		sb.WriteString("- Key files referenced: ")
		sb.WriteString(strings.Join(keyFiles, ", "))
		sb.WriteString(".\n")
	}

	// 7. Current work.
	currentWork := inferCurrentWork(messages)
	if currentWork != "" {
		sb.WriteString("- Current work: ")
		sb.WriteString(currentWork)
		sb.WriteString("\n")
	}

	// 8. Timeline.
	sb.WriteString("- Key timeline:\n")
	for _, msg := range messages {
		var parts []string
		for _, cb := range msg.Content {
			line := summarizeBlock(cb)
			if line != "" {
				parts = append(parts, line)
			}
		}
		if len(parts) > 0 {
			sb.WriteString("  - ")
			sb.WriteString(msg.Role)
			sb.WriteString(": ")
			sb.WriteString(strings.Join(parts, " | "))
			sb.WriteString("\n")
		}
	}

	sb.WriteString("</summary>")
	return sb.String()
}

// collectRecentRoleSummaries returns the most recent summaries from messages
// matching the given role, in chronological order.
func collectRecentRoleSummaries(messages []api.Message, role string, limit int) []string {
	var results []string
	for i := len(messages) - 1; i >= 0 && len(results) < limit; i-- {
		if messages[i].Role != role {
			continue
		}
		text := firstTextBlock(messages[i])
		if text == "" {
			continue
		}
		results = append(results, truncateSummary(text, 160))
	}
	// Reverse for chronological order.
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results
}

// inferPendingWork extracts messages containing work-related keywords.
func inferPendingWork(messages []api.Message) []string {
	keywords := []string{"todo", "next", "pending", "follow up", "remaining"}
	var results []string
	for i := len(messages) - 1; i >= 0 && len(results) < 3; i-- {
		text := firstTextBlock(messages[i])
		if text == "" {
			continue
		}
		lower := strings.ToLower(text)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				results = append(results, truncateSummary(text, 160))
				break
			}
		}
	}
	// Reverse for chronological order.
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	return results
}

// collectKeyFiles extracts file paths from all messages, sorted and deduplicated.
func collectKeyFiles(messages []api.Message) []string {
	var allText strings.Builder
	for _, msg := range messages {
		for _, cb := range msg.Content {
			if cb.Text != "" {
				allText.WriteString(cb.Text)
				allText.WriteString(" ")
			}
			for _, inner := range cb.Content {
				if inner.Text != "" {
					allText.WriteString(inner.Text)
					allText.WriteString(" ")
				}
			}
		}
	}
	candidates := extractFileCandidates(allText.String())
	// Sort and deduplicate.
	sort.Strings(candidates)
	var deduped []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if !seen[c] {
			seen[c] = true
			deduped = append(deduped, c)
		}
	}
	if len(deduped) > 8 {
		deduped = deduped[:8]
	}
	return deduped
}

// punctuationTrimChars is the set of characters trimmed from path candidates.
const punctuationTrimChars = ",.;:()'\"` \t\r\n"

// extractFileCandidates splits content by whitespace and returns tokens that
// look like file paths (contain "/" and have an interesting extension).
func extractFileCandidates(content string) []string {
	var results []string
	for _, token := range strings.Fields(content) {
		cleaned := strings.Trim(token, punctuationTrimChars)
		if cleaned == "" {
			continue
		}
		if strings.Contains(cleaned, "/") && hasInterestingExtension(cleaned) {
			results = append(results, cleaned)
		}
	}
	return results
}

// interestingExtensions is the set of file extensions considered interesting.
// Note: go, py, yaml, yml, toml are Go-only additions not in the Rust codebase.
// This is an intentional divergence to better support Go-centric workflows.
var interestingExtensions = map[string]bool{
	"rs": true, "ts": true, "tsx": true, "js": true, "json": true,
	"md": true, "go": true, "py": true, "yaml": true, "yml": true, "toml": true,
}

// hasInterestingExtension returns true if the path ends with a known extension.
func hasInterestingExtension(path string) bool {
	idx := strings.LastIndex(path, ".")
	if idx < 0 || idx == len(path)-1 {
		return false
	}
	ext := path[idx+1:]
	return interestingExtensions[ext]
}

// inferCurrentWork returns a truncated description of the most recent work
// by finding the last non-empty text block in the messages.
func inferCurrentWork(messages []api.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		text := firstTextBlock(messages[i])
		if text != "" {
			return truncateSummary(text, 200)
		}
	}
	return ""
}

// summarizeBlock produces a one-line summary of a content block.
func summarizeBlock(block api.ContentBlock) string {
	switch block.Type {
	case "text":
		if strings.TrimSpace(block.Text) == "" {
			return ""
		}
		return truncateSummary(strings.TrimSpace(block.Text), 160)
	case "tool_use":
		inputStr := fmt.Sprintf("%v", block.Input)
		return truncateSummary(fmt.Sprintf("tool_use %s(%s)", block.Name, inputStr), 160)
	case "tool_result":
		var output string
		for _, inner := range block.Content {
			if inner.Text != "" {
				output = inner.Text
				break
			}
		}
		prefix := "tool_result"
		if block.Name != "" {
			prefix = fmt.Sprintf("tool_result %s:", block.Name)
		}
		if block.IsError {
			return truncateSummary(fmt.Sprintf("%s error %s", prefix, output), 160)
		}
		return truncateSummary(fmt.Sprintf("%s %s", prefix, output), 160)
	default:
		return ""
	}
}

// firstTextBlock returns the text of the first text-type content block
// with non-empty trimmed text in the message.
func firstTextBlock(msg api.Message) string {
	for _, cb := range msg.Content {
		if cb.Type == "text" && strings.TrimSpace(cb.Text) != "" {
			return strings.TrimSpace(cb.Text)
		}
	}
	return ""
}

// truncateSummary truncates content to maxChars runes, appending "..." if truncated.
func truncateSummary(content string, maxChars int) string {
	if utf8.RuneCountInString(content) <= maxChars {
		return content
	}
	runes := []rune(content)
	return string(runes[:maxChars]) + "…"
}

// extractExistingCompactedSummary checks if the message is a system-role message
// with the continuation preamble, and extracts the embedded summary.
func extractExistingCompactedSummary(msg api.Message) string {
	if msg.Role != "system" {
		return ""
	}
	text := firstTextBlock(msg)
	if text == "" {
		return ""
	}
	if !strings.HasPrefix(text, compactContinuationPreamble) {
		return ""
	}
	// Strip preamble.
	extracted := strings.TrimPrefix(text, compactContinuationPreamble)
	// Strip recent messages note.
	extracted = strings.Replace(extracted, compactRecentMessagesNote, "", 1)
	// Strip direct resume instruction.
	extracted = strings.Replace(extracted, compactDirectResumeInstruction, "", 1)
	return strings.TrimSpace(extracted)
}
