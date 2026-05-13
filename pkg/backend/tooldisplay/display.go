// Package tooldisplay turns a tool call (name + raw JSON input) into the
// strings the engine renders in console logs and the per-node Tools tab.
//
// It lives in its own minimal package because two peer packages need it:
//   - pkg/backend/delegate (claude_code / codex) — formats live SDK stream
//     blocks for the run console
//   - pkg/backend/model (claw + executor) — formats in-process tool calls
//     and decides which inputs to persist in events.jsonl
//
// Two parallel name spaces are kept (CamelCase for Claude Code SDK tool
// names, snake_case for claw-code-go's built-ins) because the schemas
// behind those names differ — collapsing them would force aliasing without
// any caller asking for it. The shared logic is the value extraction +
// truncation + structured rendering (todo lists, question lists).
package tooldisplay

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CamelCaseKeys maps the CamelCase tool name surfaced by the Claude Code
// SDK (and the OpenAI-shaped codex SDK) to the ordered list of input
// fields whose value best identifies the call. First non-empty string
// wins. Tools producing structured headers (TodoWrite, AskUserQuestion)
// use sentinel keys handled by HeaderDetail below.
var CamelCaseKeys = map[string][]string{
	"Read":            {"file_path"},
	"Write":           {"file_path"},
	"Edit":            {"file_path"},
	"MultiEdit":       {"file_path"},
	"NotebookEdit":    {"notebook_path", "file_path"},
	"Bash":            {"command"},
	"BashOutput":      {"bash_id"},
	"KillShell":       {"shell_id"},
	"Glob":            {"pattern"},
	"Grep":            {"pattern"},
	"WebFetch":        {"url"},
	"WebSearch":       {"query"},
	"Task":            {"description"},
	"TodoWrite":       {sentinelTodos},
	"ToolSearch":      {"query"},
	"SlashCommand":    {"command_name", "command"},
	"AskUserQuestion": {sentinelQuestions},
	"ScheduleWakeup":  {"reason"},
}

// SnakeCaseKeys mirrors CamelCaseKeys for claw-code-go's snake_case names
// and the legacy iterion built-ins (mcp-style tool names already strip
// their `mcp__server__` prefix elsewhere).
var SnakeCaseKeys = map[string][]string{
	"read_file":     {"path", "file_path"},
	"file_edit":     {"path", "file_path"},
	"write_file":    {"path", "file_path"},
	"notebook_edit": {"path", "file_path", "notebook_path"},
	"bash":          {"command"},
	"grep":          {"pattern"},
	"glob":          {"pattern"},
	"web_fetch":     {"url"},
	"web_search":    {"query"},
	"skill":         {"skill", "name"},
	"agent":         {"description"},
	"ask_user":      {"question"},
	"task_create":   {"description"},
	"tool_search":   {"query"},
	"sleep":         {"seconds", "duration"},
	"todo_write":    {sentinelTodos},
	"task":          {"description", "prompt"},
	"slash_command": {"command_name", "command"},
}

const (
	sentinelTodos     = "_todos_summary"
	sentinelQuestions = "_questions_summary"
)

// fallbackKeys is the priority order tried when the tool name is not in
// either dispatch map. It matches the long-standing pre-refactor behavior
// of the claude_code delegate so unknown tools degrade to the same one-line
// detail they used to produce.
var fallbackKeys = []string{"file_path", "path", "pattern", "command"}

// StructuredInputTools is the whitelist of tools whose JSON input is
// always persisted in the `tool_started` event payload (in addition to
// the bare name + size). Used by the editor's per-node Tools tab to
// render rich cards (todo lists, web fetches, search queries). Other
// tools still log their detail to console but keep the event payload
// minimal — Bash/Read can carry MB of content and would bloat events.jsonl.
//
// Both name spaces are listed because the same event consumer (UI)
// receives events from both backends, and `claude_code` uses CamelCase
// names whereas `claw` uses snake_case.
var StructuredInputTools = map[string]struct{}{
	"TodoWrite":       {},
	"AskUserQuestion": {},
	"Task":            {},
	"WebFetch":        {},
	"WebSearch":       {},
	"ToolSearch":      {},
	"Grep":            {},
	"Glob":            {},
	"NotebookEdit":    {},

	"todo_write":    {},
	"ask_user":      {},
	"task":          {},
	"web_fetch":     {},
	"web_search":    {},
	"tool_search":   {},
	"grep":          {},
	"glob":          {},
	"notebook_edit": {},
	"agent":         {},
}

// IsStructured reports whether the given tool name is on the whitelist of
// tools whose input is persisted in the tool_started event payload.
func IsStructured(toolName string) bool {
	_, ok := StructuredInputTools[toolName]
	return ok
}

// HeaderDetail returns the single-line detail string appended after the
// tool name in console logs, e.g. "🔧 WebFetch https://example.com/api".
// Returns "" when no informative argument can be extracted.
//
// keys selects the dispatch map (CamelCaseKeys for delegate sites,
// SnakeCaseKeys for in-process claw tool calls). The fallbackKeys priority
// is tried last so unknown / custom tools still surface their target when
// they happen to use a conventional argument name.
func HeaderDetail(toolName string, input []byte, keys map[string][]string) string {
	if len(input) == 0 {
		return ""
	}
	var raw map[string]any
	if err := json.Unmarshal(input, &raw); err != nil {
		return ""
	}
	tryKeys := keys[toolName]
	if len(tryKeys) == 0 {
		tryKeys = fallbackKeys
	}
	for _, k := range tryKeys {
		switch k {
		case sentinelTodos:
			if s := summarizeTodosOneLine(raw["todos"]); s != "" {
				return s
			}
		case sentinelQuestions:
			if s := summarizeQuestionsOneLine(raw["questions"]); s != "" {
				return s
			}
		default:
			if s := stringFromInput(raw[k]); s != "" {
				return truncate(firstLine(s), 100)
			}
		}
	}
	return ""
}

// BlockBody returns a multi-line body to attach under the log header for
// tools where the operator typically wants the full content (multi-line
// Bash commands, TodoWrite task lists). Empty when the header already
// says it all — the logger's LogBlock then skips the continuation lines.
func BlockBody(toolName string, input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var raw map[string]any
	if err := json.Unmarshal(input, &raw); err != nil {
		return ""
	}
	switch toolName {
	case "TodoWrite", "todo_write":
		return formatTodoList(raw["todos"])
	case "AskUserQuestion":
		return formatQuestionList(raw["questions"])
	}
	if c, ok := raw["command"].(string); ok && strings.ContainsRune(c, '\n') {
		return c
	}
	if s, ok := raw["script"].(string); ok && strings.ContainsRune(s, '\n') {
		return s
	}
	return ""
}

// stringFromInput coerces a JSON value to its display string, returning
// "" for arrays, maps, and nil so the caller falls through to the next
// candidate key.
func stringFromInput(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return fmt.Sprintf("%g", s)
	case bool:
		return fmt.Sprintf("%v", s)
	}
	return ""
}

// summarizeTodosOneLine produces a compact summary for the log header:
// "4 todos, ★ <in_progress content>, ☑ 1 done, ☐ 2 pending". The
// in_progress task is surfaced because it is the only one the agent is
// actively working on; the rest are aggregated by status with the same
// glyphs the multi-line body uses (☐ pending, ★ in-progress, ☑ done).
func summarizeTodosOneLine(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	var inProgress string
	pending, done := 0, 0
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		status, _ := m["status"].(string)
		switch status {
		case "in_progress":
			if inProgress == "" {
				inProgress = content
			}
		case "completed", "done":
			done++
		default:
			pending++
		}
	}
	parts := []string{fmt.Sprintf("%d todos", len(items))}
	if inProgress != "" {
		parts = append(parts, fmt.Sprintf("★ %s", truncate(firstLine(inProgress), 60)))
	}
	if done > 0 {
		parts = append(parts, fmt.Sprintf("☑ %d done", done))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("☐ %d pending", pending))
	}
	return truncate(strings.Join(parts, ", "), 120)
}

// formatTodoList renders the full task list as a multi-line block,
// modelled on Claude Code's own console convention: empty box for
// pending, star for in_progress ("the checkbox is filled with a star
// as the active marker"), checked box for completed. The terminal
// rendering uses single-glyph markers so column alignment is preserved
// across rows; the editor's TodoChecklist React component overlays the
// star inside the ☐ for the equivalent visual.
//
//	☐ Set up project structure
//	★ Implement core feature
//	☑ Write tests
func formatTodoList(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		status, _ := m["status"].(string)
		var glyph string
		switch status {
		case "in_progress":
			glyph = "★"
		case "completed", "done":
			glyph = "☑"
		default:
			glyph = "☐"
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s %s", glyph, truncate(firstLine(content), 200))
	}
	return b.String()
}

// summarizeQuestionsOneLine produces a header detail for AskUserQuestion:
// the first question's text plus a count when there are more.
func summarizeQuestionsOneLine(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	first, _ := items[0].(map[string]any)
	if first == nil {
		return ""
	}
	q, _ := first["question"].(string)
	if q == "" {
		return ""
	}
	if len(items) == 1 {
		return truncate(firstLine(q), 100)
	}
	return fmt.Sprintf("%s (+%d more)", truncate(firstLine(q), 80), len(items)-1)
}

// formatQuestionList renders each question on its own line.
func formatQuestionList(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		q, _ := m["question"].(string)
		if q == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d. %s", i+1, truncate(firstLine(q), 200))
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
