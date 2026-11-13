package runtime

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestFormatCompactSummaryStripsAnalysisTags(t *testing.T) {
	input := "<analysis>Some internal thinking</analysis>\n<summary>The actual summary content</summary>"
	result := FormatCompactSummary(input)

	if strings.Contains(result, "internal thinking") {
		t.Error("analysis tags should be stripped")
	}
	if !strings.Contains(result, "The actual summary content") {
		t.Error("summary content should be preserved")
	}
	if strings.Contains(result, "<compacted_context>") {
		t.Error("should NOT be wrapped in compacted_context tags")
	}
	if !strings.Contains(result, "Summary:") {
		t.Error("should contain Summary: prefix")
	}
}

func TestFormatCompactSummaryMatchesRust(t *testing.T) {
	// Matches Rust test: formats_compact_summary_like_upstream
	summary := "<analysis>scratch</analysis>\n<summary>Kept work</summary>"
	result := FormatCompactSummary(summary)
	if result != "Summary:\nKept work" {
		t.Errorf("format mismatch: got %q, want %q", result, "Summary:\nKept work")
	}
}

func TestFormatCompactSummaryNoTags(t *testing.T) {
	input := "Plain summary without tags"
	result := FormatCompactSummary(input)
	if !strings.Contains(result, "Plain summary without tags") {
		t.Error("plain text should be preserved")
	}
}

func TestFormatCompactSummaryExtractsSummaryTag(t *testing.T) {
	input := "Some preamble\n<summary>\nExtracted content\n</summary>\nSome epilogue"
	result := FormatCompactSummary(input)
	if !strings.Contains(result, "Extracted content") {
		t.Error("should extract summary tag content")
	}
	if !strings.Contains(result, "Summary:") {
		t.Error("should contain Summary: prefix")
	}
	if strings.Contains(result, "<compacted_context>") {
		t.Error("should NOT be wrapped in compacted_context tags")
	}
}

func TestGetContinuationMessageSuppressFollowUp(t *testing.T) {
	msg := GetContinuationMessage("summary text", true, false)
	// Wire-safe shape: injected user message, never role "system" (the
	// Anthropic Messages API rejects system entries inside messages[]).
	if msg.Role != "user" {
		t.Errorf("Role = %q, want 'user'", msg.Role)
	}
	if !msg.IsInjected {
		t.Error("continuation must be IsInjected so it stays out of the real-turn count")
	}
	text := msg.Content[0].Text
	if !strings.Contains(text, "do not acknowledge the summary") {
		t.Error("suppressFollowUp should include direct resume instruction")
	}
	if !strings.Contains(text, "summary text") {
		t.Error("should include summary")
	}
}

func TestGetContinuationMessageNoSuppress(t *testing.T) {
	msg := GetContinuationMessage("summary text", false, false)
	text := msg.Content[0].Text
	if strings.Contains(text, "do not acknowledge") {
		t.Error("should not include suppress instruction when not suppressed")
	}
}

func TestGetContinuationMessageRecentPreserved(t *testing.T) {
	msg := GetContinuationMessage("summary", true, true)
	text := msg.Content[0].Text
	if !strings.Contains(text, "Recent messages are preserved verbatim") {
		t.Error("should include recent messages note")
	}
}

func TestGetContinuationMessageFormatsInternally(t *testing.T) {
	// GetContinuationMessage should call FormatCompactSummary on its input,
	// matching Rust's get_compact_continuation_message behavior.
	summary := "<analysis>scratch</analysis>\n<summary>Real content</summary>"
	msg := GetContinuationMessage(summary, false, false)
	text := msg.Content[0].Text
	if strings.Contains(text, "<analysis>") {
		t.Error("analysis tags should be stripped by GetContinuationMessage")
	}
	if !strings.Contains(text, "Real content") {
		t.Error("summary content should be preserved")
	}
}

func TestMergeCompactSummaries(t *testing.T) {
	result := MergeCompactSummaries("first summary", "second summary")
	if !strings.Contains(result, "first summary") || !strings.Contains(result, "second summary") {
		t.Error("should contain both summaries")
	}
	if !strings.Contains(result, "Previously compacted context:") {
		t.Error("should contain 'Previously compacted context:' section")
	}
	if !strings.Contains(result, "Newly compacted context:") {
		t.Error("should contain 'Newly compacted context:' section")
	}
}

func TestMergeCompactSummariesRustFormat(t *testing.T) {
	// Verify the merged format matches Rust: Conversation summary header,
	// "- " prefix on sections, "  " indentation on content.
	result := MergeCompactSummaries("first", "second")
	if !strings.Contains(result, "Conversation summary:") {
		t.Error("should contain 'Conversation summary:' header")
	}
	if !strings.Contains(result, "- Previously compacted context:") {
		t.Error("should have '- ' prefix on Previously compacted")
	}
	if !strings.Contains(result, "- Newly compacted context:") {
		t.Error("should have '- ' prefix on Newly compacted")
	}
	if !strings.Contains(result, "  first") {
		t.Error("content should be indented with two spaces")
	}
	if !strings.Contains(result, "  second") {
		t.Error("content should be indented with two spaces")
	}
}

func TestMergeCompactSummariesStripsAnalysisTags(t *testing.T) {
	// BLOCKER FIX: MergeCompactSummaries must format current before extraction
	// to prevent <analysis> tag leaks (matching Rust behavior).
	current := "<analysis>internal thinking</analysis>\n<summary>Actual summary\n- Key timeline:\n  - user: did something</summary>"
	result := MergeCompactSummaries("previous context", current)

	if strings.Contains(result, "internal thinking") {
		t.Error("analysis tags should be stripped from current before merging")
	}
	if strings.Contains(result, "<analysis>") {
		t.Error("analysis tags should not appear in merged output")
	}
	if !strings.Contains(result, "Actual summary") {
		t.Error("should preserve summary content after stripping analysis")
	}
}

func TestMergeCompactSummariesPreservesTimeline(t *testing.T) {
	current := "<summary>Conversation summary:\n- Scope: 5 messages.\n- Key timeline:\n  - user: asked question\n  - assistant: answered</summary>"
	result := MergeCompactSummaries("prior context", current)

	if !strings.Contains(result, "- Key timeline:") {
		t.Error("should preserve timeline section")
	}
	if !strings.Contains(result, "user: asked question") {
		t.Error("should preserve timeline entries")
	}
}

func TestCollapseBlankLines(t *testing.T) {
	input := "line1\n\n\n\nline2\n\n\n\n\nline3"
	result := collapseBlankLines(input)
	expected := "line1\n\nline2\n\nline3"
	if result != expected {
		t.Errorf("collapseBlankLines = %q, want %q", result, expected)
	}
}

func TestMergeCompactSummariesEmpty(t *testing.T) {
	if MergeCompactSummaries("", "current") != "current" {
		t.Error("empty previous should return current")
	}
	if MergeCompactSummaries("previous", "") != "previous" {
		t.Error("empty current should return previous")
	}
	if MergeCompactSummaries("", "") != "" {
		t.Error("both empty should return empty")
	}
}

func TestRepeatedCompactionPreservesContext(t *testing.T) {
	// Simulate two rounds of compaction to verify the merged summary used
	// in the continuation message includes all prior compacted context.
	firstSummary := "<summary>Conversation summary:\n- Scope: 3 messages.\n- Key timeline:\n  - user: initial request</summary>"
	secondSummary := "<summary>Conversation summary:\n- Scope: 2 messages.\n- Key timeline:\n  - user: follow-up</summary>"

	// First merge
	merged1 := MergeCompactSummaries("", firstSummary)
	if merged1 != firstSummary {
		t.Error("first merge with empty previous should return current")
	}

	// Second merge uses first result as previous
	merged2 := MergeCompactSummaries(merged1, secondSummary)

	if !strings.Contains(merged2, "Previously compacted context:") {
		t.Error("second merge should contain previously compacted context")
	}
	if !strings.Contains(merged2, "Newly compacted context:") {
		t.Error("second merge should contain newly compacted context")
	}
	// Verify first summary content is in the "previously compacted" section
	if !strings.Contains(merged2, "Scope: 3 messages") {
		t.Error("first summary scope should be preserved in previously compacted context")
	}
	// Verify second summary content is in the "newly compacted" section
	if !strings.Contains(merged2, "Scope: 2 messages") {
		t.Error("second summary scope should appear in newly compacted context")
	}
}

// ---------------------------------------------------------------------------
// Pure-function compaction tests
// ---------------------------------------------------------------------------

func makeTextMsg(role, text string) api.Message {
	return api.Message{
		Role:    role,
		Content: []api.ContentBlock{{Type: "text", Text: text}},
	}
}

func makeToolUseMsg(role, toolName string) api.Message {
	return api.Message{
		Role: role,
		Content: []api.ContentBlock{{
			Type:  "tool_use",
			Name:  toolName,
			Input: map[string]any{"arg": "value"},
		}},
	}
}

func makeToolResultMsg(role, output string, isError bool) api.Message {
	return api.Message{
		Role: role,
		Content: []api.ContentBlock{{
			Type:    "tool_result",
			IsError: isError,
			Content: []api.ContentBlock{{Type: "text", Text: output}},
		}},
	}
}

func TestSummarizeMessagesRoleCountsAndTools(t *testing.T) {
	messages := []api.Message{
		makeTextMsg("user", "Please fix the bug"),
		makeToolUseMsg("assistant", "read_file"),
		makeToolResultMsg("tool", "file contents here", false),
		makeTextMsg("assistant", "I found the issue"),
		makeTextMsg("user", "Great, apply the fix"),
		makeToolUseMsg("assistant", "write_file"),
	}

	summary := SummarizeMessages(messages)

	if !strings.Contains(summary, "<summary>") || !strings.Contains(summary, "</summary>") {
		t.Error("should be wrapped in summary tags")
	}
	if !strings.Contains(summary, "user=2") {
		t.Error("should count 2 user messages")
	}
	if !strings.Contains(summary, "assistant=3") {
		t.Error("should count 3 assistant messages")
	}
	if !strings.Contains(summary, "tool=1") {
		t.Error("should count 1 tool message")
	}
	if !strings.Contains(summary, "read_file") {
		t.Error("should mention read_file tool")
	}
	if !strings.Contains(summary, "write_file") {
		t.Error("should mention write_file tool")
	}
	if !strings.Contains(summary, "Key timeline:") {
		t.Error("should contain timeline section")
	}
}

func TestMergeCompactSummariesEmptyPreviousUnchanged(t *testing.T) {
	newSummary := "<summary>\nConversation summary:\n- Scope: 5 messages.\n</summary>"
	result := MergeCompactSummaries("", newSummary)
	if result != newSummary {
		t.Errorf("empty previous should return new unchanged, got %q", result)
	}
}

func TestCompactSessionPureBelowThreshold(t *testing.T) {
	messages := []api.Message{
		makeTextMsg("user", "hello"),
		makeTextMsg("assistant", "hi"),
	}
	cfg := DefaultCompactionConfig()
	result := CompactSessionPure(messages, cfg)
	if result != nil {
		t.Error("should return nil when below threshold")
	}
}

func TestCompactSessionPureAboveThreshold(t *testing.T) {
	// Generate enough messages to exceed 10000 estimated tokens.
	longText := strings.Repeat("word ", 2000) // 2000*5=10000 chars => ~2500 tokens per message
	messages := make([]api.Message, 0, 10)
	for i := 0; i < 10; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, makeTextMsg(role, longText))
	}

	cfg := DefaultCompactionConfig()
	result := CompactSessionPure(messages, cfg)
	if result == nil {
		t.Fatal("should return CompactionResult when above threshold")
	}
	if result.Summary == "" {
		t.Error("summary should not be empty")
	}
	if result.FormattedSummary == "" {
		t.Error("formatted summary should not be empty")
	}
	if len(result.CompactedMessages) == 0 {
		t.Error("compacted messages should not be empty")
	}
	// Should have continuation message + preserved recent messages.
	if first := result.CompactedMessages[0]; first.Role != "user" || !first.IsInjected {
		t.Errorf("first compacted message should be the injected user continuation, got role=%q injected=%v", first.Role, first.IsInjected)
	}
	if result.RemovedMessageCount <= 0 {
		t.Error("should have removed some messages")
	}
	// Recent messages should be preserved.
	recentCount := len(result.CompactedMessages) - 1 // minus continuation msg
	if recentCount != cfg.PreserveRecentMessages {
		t.Errorf("should preserve %d recent messages, got %d", cfg.PreserveRecentMessages, recentCount)
	}
}

func TestCompactSessionPureWithExistingCompactedSummary(t *testing.T) {
	// Create a continuation message as first message (simulating prior compaction).
	existingSummary := "<summary>\nConversation summary:\n- Scope: 3 earlier messages.\n</summary>"
	continuationMsg := GetContinuationMessage(existingSummary, true, true)

	longText := strings.Repeat("content ", 2000)
	messages := []api.Message{continuationMsg}
	for i := 0; i < 10; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, makeTextMsg(role, longText))
	}

	cfg := DefaultCompactionConfig()
	result := CompactSessionPure(messages, cfg)
	if result == nil {
		t.Fatal("should return CompactionResult")
	}
	// Should merge with existing summary.
	if !strings.Contains(result.Summary, "Previously compacted context:") {
		t.Error("merged summary should contain previously compacted context")
	}
	if !strings.Contains(result.Summary, "Newly compacted context:") {
		t.Error("merged summary should contain newly compacted context")
	}
}

func TestInferPendingWork(t *testing.T) {
	messages := []api.Message{
		makeTextMsg("user", "We need to fix this bug"),
		makeTextMsg("assistant", "Here's the plan:\n- TODO: update the config\n- Next: run the tests\n- Also remaining: clean up"),
		makeTextMsg("user", "Sounds good"),
	}
	pending := inferPendingWork(messages)
	if len(pending) == 0 {
		t.Fatal("should find pending work items")
	}
	found := false
	for _, p := range pending {
		if strings.Contains(strings.ToLower(p), "todo") || strings.Contains(strings.ToLower(p), "next") || strings.Contains(strings.ToLower(p), "remaining") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("should detect todo/next/remaining keywords, got: %v", pending)
	}
}

func TestCollectKeyFiles(t *testing.T) {
	messages := []api.Message{
		makeTextMsg("user", "Check src/main.go and config/app.yaml"),
		makeTextMsg("assistant", "I see issues in lib/utils.ts and tests/test.py"),
	}
	files := collectKeyFiles(messages)
	if len(files) == 0 {
		t.Fatal("should extract file paths")
	}
	expectedFiles := []string{"src/main.go", "config/app.yaml", "lib/utils.ts", "tests/test.py"}
	for _, expected := range expectedFiles {
		found := false
		for _, f := range files {
			if f == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("should find %q in extracted files: %v", expected, files)
		}
	}
}

func TestTruncateSummaryRuneBased(t *testing.T) {
	// ASCII case.
	short := "hello"
	if truncateSummary(short, 10) != "hello" {
		t.Error("should not truncate short strings")
	}
	long := "hello world"
	result := truncateSummary(long, 5)
	if result != "hello…" {
		t.Errorf("truncate ASCII: got %q, want %q", result, "hello…")
	}

	// Unicode case — each character is one rune.
	unicode := "日本語のテスト文字列"
	result = truncateSummary(unicode, 4)
	if result != "日本語の…" {
		t.Errorf("truncate unicode: got %q, want %q", result, "日本語の…")
	}
}

func TestExtractExistingCompactedSummary(t *testing.T) {
	summary := "<summary>Test summary content</summary>"
	msg := GetContinuationMessage(summary, true, true)

	extracted := extractExistingCompactedSummary(msg)
	if extracted == "" {
		t.Fatal("should extract summary from continuation message")
	}
	if !strings.Contains(extracted, "Test summary content") {
		t.Errorf("extracted summary should contain original content, got %q", extracted)
	}
}

func TestExtractExistingCompactedSummaryNonSystem(t *testing.T) {
	msg := makeTextMsg("user", "not a system message")
	if extractExistingCompactedSummary(msg) != "" {
		t.Error("should return empty for non-system messages")
	}
}

func TestExtractExistingCompactedSummaryNoPreamble(t *testing.T) {
	msg := api.Message{
		Role:    "system",
		Content: []api.ContentBlock{{Type: "text", Text: "Just a regular system message"}},
	}
	if extractExistingCompactedSummary(msg) != "" {
		t.Error("should return empty for system messages without continuation preamble")
	}
}

func TestInferPendingWorkMatchesRust(t *testing.T) {
	// Matches Rust test: infers_pending_work_from_recent_messages
	messages := []api.Message{
		makeTextMsg("user", "done"),
		makeTextMsg("assistant", "Next: update tests and follow up on remaining CLI polish."),
	}
	pending := inferPendingWork(messages)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending item, got %d: %v", len(pending), pending)
	}
	if !strings.Contains(pending[0], "Next: update tests") {
		t.Errorf("pending[0] = %q, want to contain 'Next: update tests'", pending[0])
	}
}

func TestCollectKeyFilesMatchesRust(t *testing.T) {
	// Matches Rust test: extracts_key_files_from_message_content
	files := collectKeyFiles([]api.Message{
		makeTextMsg("user", "Update rust/crates/runtime/src/compact.rs and rust/crates/rusty-claude-cli/src/main.rs next."),
	})
	found := map[string]bool{}
	for _, f := range files {
		found[f] = true
	}
	if !found["rust/crates/runtime/src/compact.rs"] {
		t.Error("should find compact.rs")
	}
	if !found["rust/crates/rusty-claude-cli/src/main.rs"] {
		t.Error("should find main.rs")
	}
}

func TestTruncatesLongBlocksInSummary(t *testing.T) {
	// Matches Rust: truncates_long_blocks_in_summary
	block := api.ContentBlock{Type: "text", Text: strings.Repeat("x", 400)}
	summary := summarizeBlock(block)
	if !strings.HasSuffix(summary, "…") {
		t.Error("should end with ellipsis")
	}
	if len([]rune(summary)) > 161 {
		t.Errorf("should be <= 161 runes, got %d", len([]rune(summary)))
	}
}

func TestEstimateTokensTextOnly(t *testing.T) {
	messages := []api.Message{
		makeTextMsg("user", "hello world"), // 11 chars / 4 + 1 = 3
	}
	tokens := EstimateTokens(messages)
	if tokens != 3 {
		t.Errorf("EstimateTokens text = %d, want 3", tokens)
	}
}

func TestEstimateTokensToolUse(t *testing.T) {
	// Rust: (name.len() + input_json.len()) / 4 + 1
	messages := []api.Message{
		makeToolUseMsg("assistant", "read_file"), // name=9, input={"arg":"value"} ~15 chars
	}
	tokens := EstimateTokens(messages)
	// Should be (9 + len(json)) / 4 + 1, not just text-based
	if tokens <= 0 {
		t.Errorf("EstimateTokens tool_use = %d, want > 0", tokens)
	}
}

func TestEstimateTokensToolResult(t *testing.T) {
	// Rust: (tool_name.len() + output.len()) / 4 + 1
	messages := []api.Message{
		makeToolResultMsg("tool", "file contents here", false),
	}
	tokens := EstimateTokens(messages)
	if tokens <= 0 {
		t.Errorf("EstimateTokens tool_result = %d, want > 0", tokens)
	}
}

func TestEstimateTokensMixedBlocks(t *testing.T) {
	messages := []api.Message{
		makeTextMsg("user", "Please fix the bug"),
		makeToolUseMsg("assistant", "read_file"),
		makeToolResultMsg("tool", "file contents here", false),
		makeTextMsg("assistant", "I found the issue"),
	}
	tokens := EstimateTokens(messages)
	if tokens < 4 {
		t.Errorf("EstimateTokens mixed = %d, want >= 4 (one per block minimum)", tokens)
	}
}

func TestSummarizeMessagesTimelineJoinsBlocks(t *testing.T) {
	// Verify timeline entries join multiple blocks with " | " (matching Rust)
	messages := []api.Message{
		{
			Role: "assistant",
			Content: []api.ContentBlock{
				{Type: "text", Text: "thinking about it"},
				{Type: "tool_use", Name: "bash", Input: map[string]any{"cmd": "ls"}},
			},
		},
	}
	summary := SummarizeMessages(messages)
	// Should contain a single timeline entry with " | " joining the two blocks
	if !strings.Contains(summary, "assistant: thinking about it | tool_use bash(") {
		t.Errorf("timeline should join blocks with ' | ', got:\n%s", summary)
	}
}
