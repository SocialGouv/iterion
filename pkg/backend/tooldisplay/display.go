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
	"unicode/utf8"
)

// outputBodyMax bounds the tool-result body attached to a log block. The full
// result still lives on the structured tool_called event (sidecar-blob
// paginated in the studio Tools tab); the log keeps a generous but bounded
// slice so a multi-MB command dump can't blow the in-memory 1 MB log ring.
const outputBodyMax = 4096

// ResultDisplay splits a tool result string into a one-line header detail
// (truncated to headerDetailMax) and an expandable body. The body is the full
// result — bounded to outputBodyMax — whenever the header can't show it whole
// (multi-line, or longer than the header budget); "" when the header already
// says it all, so short results stay one-liners with no needless "▸ expand".
//
// Shared by the claude_code and claw tool-result log paths so a tool's output
// renders identically on both backends (claw⇄claude_code parity).
func ResultDisplay(output string) (header, body string) {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return "", ""
	}
	header = truncate(firstLine(output), headerDetailMax)
	if strings.ContainsRune(output, '\n') || len(firstLine(output)) > headerDetailMax {
		body = output
		if len(body) > outputBodyMax {
			b := body[:outputBodyMax]
			for len(b) > 0 && !utf8.RuneStart(b[len(b)-1]) {
				b = b[:len(b)-1]
			}
			body = b + "\n… (truncated — full result in the run's Tools tab)"
		}
	}
	return header, body
}

// CamelCaseKeys maps the CamelCase tool name surfaced by the Claude Code
// SDK (and the OpenAI-shaped codex SDK) to the ordered list of input
// fields whose value best identifies the call. First non-empty string
// wins. Tools producing structured headers (TodoWrite, AskUserQuestion,
// Task/Agent sub-agent dispatch) use sentinel keys handled by
// HeaderDetail below.
//
// Both "Task" (legacy Claude Code SDK name) and "Agent" (current SDK
// name) are registered because the SDK alias has shifted across
// versions and the live name flows straight from `ToolUseBlock.Name`.
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
	"Task":            {sentinelAgent},
	"Agent":           {sentinelAgent},
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
	"read_file":  {"path", "file_path"},
	"file_edit":  {"path", "file_path"},
	"write_file": {"path", "file_path"},
	// pi's built-ins are bare verbs with a `path` argument. Without these the
	// run console renders a pi node's file operations with no target at all.
	"read":          {"path", "file_path"},
	"edit":          {"path", "file_path"},
	"write":         {"path", "file_path"},
	"find":          {"pattern"},
	"ls":            {"path"},
	"notebook_edit": {"path", "file_path", "notebook_path"},
	"bash":          {"command"},
	"grep":          {"pattern"},
	"glob":          {"pattern"},
	"web_fetch":     {"url"},
	"web_search":    {"query"},
	"skill":         {"skill", "name"},
	"agent":         {sentinelAgent},
	"ask_user":      {"question"},
	"task_create":   {"description"},
	"tool_search":   {"query"},
	"sleep":         {"seconds", "duration"},
	"todo_write":    {sentinelTodos},
	"task":          {sentinelAgent},
	"slash_command": {"command_name", "command"},
}

const (
	sentinelTodos     = "_todos_summary"
	sentinelQuestions = "_questions_summary"
	sentinelAgent     = "_agent_summary"
)

// fallbackKeys is the priority order tried when the tool name is not in
// either dispatch map. It matches the long-standing pre-refactor behavior
// of the claude_code delegate so unknown tools degrade to the same one-line
// detail they used to produce.
var fallbackKeys = []string{"file_path", "path", "pattern", "command"}

// headerDetailMax is the byte budget for the single-line header detail.
// Shared by HeaderDetail (which truncates to it) and BlockBody (which
// surfaces the full value as an expandable body once the header exceeds it),
// so the "clip in the header, expand for the whole thing" contract stays in
// one place.
const headerDetailMax = 100

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
		case sentinelAgent:
			if s := summarizeAgentOneLine(raw); s != "" {
				return s
			}
		default:
			if s := stringFromInput(raw[k]); s != "" {
				return truncate(firstLine(shortenWorktreePath(s)), headerDetailMax)
			}
		}
	}
	return ""
}

// shortenWorktreePath strips the ephemeral run-worktree prefix
// (".../worktrees/<run-id>/") from a bare absolute path so a tool-log
// header shows the workspace-relative path (e.g. "e2e/foo.go") instead of a
// ~95-char prefix that wraps in narrow panes and reads as duplicated
// fragments. Deliberately conservative: only rewrites a value that is a bare
// absolute path (leading "/", no whitespace) containing the "/worktrees/"
// segment, so Bash command strings, URLs, and patterns pass through untouched.
func shortenWorktreePath(s string) string {
	const marker = "/worktrees/"
	if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \t") {
		return s
	}
	i := strings.Index(s, marker)
	if i < 0 {
		return s
	}
	rest := s[i+len(marker):] // "<run-id>/e2e/foo.go"
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[j+1:] // drop the run-id segment → "e2e/foo.go"
	}
	return s
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
	case "Agent", "Task", "agent", "task":
		if p, ok := raw["prompt"].(string); ok && p != "" {
			return p
		}
		return ""
	}
	// Generic: when the one-line header had to clip (multi-line) or truncate
	// (over headerDetailMax) the tool's primary argument — a long single-line
	// Bash command, a multi-line script, an over-long path/pattern — surface
	// the FULL value as the expandable body so the operator can read it whole
	// in-context (the studio's LogBlock renders it under a "▸ expand"). Empty
	// when the header already shows everything, so short calls stay one-liners.
	for _, k := range fallbackKeys {
		s, ok := raw[k].(string)
		if !ok || s == "" {
			continue
		}
		if strings.ContainsRune(s, '\n') || len(firstLine(shortenWorktreePath(s))) > headerDetailMax {
			return s
		}
		return ""
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
// across rows; the studio's TodoChecklist React component overlays the
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

// summarizeAgentOneLine produces a header detail for Agent/Task tool
// calls that combines the sub-agent type with the short description so
// the operator can tell at a glance which sub-agent was dispatched and
// for what. Falls back gracefully: missing subagent_type yields just
// the description, missing description yields just the subagent_type,
// and missing both returns "" (caller renders the bare tool name).
func summarizeAgentOneLine(raw map[string]any) string {
	sub, _ := raw["subagent_type"].(string)
	desc, _ := raw["description"].(string)
	sub = firstLine(strings.TrimSpace(sub))
	desc = firstLine(strings.TrimSpace(desc))
	switch {
	case sub != "" && desc != "":
		return truncate(fmt.Sprintf("%s: %s", sub, desc), 120)
	case sub != "":
		return truncate(sub, 120)
	case desc != "":
		return truncate(desc, 120)
	}
	return ""
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
