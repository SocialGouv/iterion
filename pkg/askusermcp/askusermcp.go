// Package askusermcp holds the ask-user MCP tool surface shared by the
// two transports that expose it to a claude_code session:
//
//   - the stdio server (`iterion __mcp-ask-user`,
//     cmd/iterion/mcp_ask_user.go) used for unsandboxed runs, and
//   - the per-run HTTP endpoint ([Handler]) the engine binds next to a
//     sandbox so the in-container claude CLI can reach the same tools
//     (ADR-082 Phase 3 blocker 2 — mirrors the board MCP HTTP
//     transport at /api/v1/mcp/board).
//
// Both transports advertise the same three tools (ask_user,
// ask_user_async, await_answers — ADR-081) with identical schemas and
// return identical tools/call results. The REAL interaction semantics
// live in the claude_code delegate's PreToolUse hooks, which run
// host-side over the SDK control channel and are therefore
// transport-independent: ask_user / await_answers are intercepted
// there (pause escalation), ask_user_async is persisted there via the
// executor's Task.PostAsyncQuestion closure (store.WriteInteraction /
// AnswerInteraction) and then allowed through. The server-side
// tools/call is a defensive fallback for the intercepted pair and the
// canned keep-working success text for ask_user_async.
package askusermcp

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// AskUserToolName is the blocking ask_user tool. The async pair's
// names live in pkg/backend/delegate (AskUserAsyncToolName /
// AwaitAnswersToolName) because the delegate builds their
// fully-qualified mcp__iterion__* forms from them.
const AskUserToolName = "ask_user"

// askUserInputSchema is the JSON Schema for the ask_user and
// ask_user_async tool inputs. Mirrors claw-code-go's native ask_user
// tool shape (options + free text) so both backends offer the LLM the
// same structured contract.
var askUserInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "The clarifying question to ask the human user."
    },
    "options": {
      "type": "array",
      "description": "Optional list of selectable answers rendered as clickable choices. Each option must have an id and a label.",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "description": "Stable identifier returned to the model."},
          "label": {"type": "string", "description": "Human-readable text shown to the user."}
        },
        "required": ["id", "label"],
        "additionalProperties": false
      }
    },
    "allow_free_text": {
      "type": "boolean",
      "description": "When true (default if no options are provided), the user may type a free-text response instead of selecting an option."
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`)

// awaitAnswersInputSchema is the JSON Schema for the await_answers
// tool input (an optional note; the real state lives in the run's
// interaction store, consulted by the PreToolUse hook).
var awaitAnswersInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "note": {
      "type": "string",
      "description": "Optional short note on why you need the answers now (shown to the operator)."
    }
  },
  "additionalProperties": false
}`)

// Tool is one advertised MCP tool (the tools/list entry shape).
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Tools returns the ask-user tool set both transports advertise, in
// stable order.
func Tools() []Tool {
	return []Tool{
		{
			Name:        AskUserToolName,
			Description: "Pause execution and ask the human running this workflow a clarifying question. Use this when you need information, approval, or guidance you cannot derive yourself. Optional `options` present the user with clickable choices; when `allow_free_text` is true the user may also type a free response.",
			InputSchema: askUserInputSchema,
		},
		{
			Name:        delegate.AskUserAsyncToolName,
			Description: "Ask the human operator a question WITHOUT pausing your work. The question is posted immediately and you keep working; the operator's answer arrives later in your conversation as an operator message tagged with the question id. Post questions as early as possible. Use ask_user instead when you cannot take another step without the answer.",
			InputSchema: askUserInputSchema,
		},
		{
			Name:        delegate.AwaitAnswersToolName,
			Description: "Wait for the operator's answers to the questions you posted with ask_user_async. Call this ONLY when you truly cannot proceed without the pending answers. If everything is already answered it returns the answers immediately; otherwise the run pauses until the operator replies.",
			InputSchema: awaitAnswersInputSchema,
		},
	}
}

// CallResult builds the tools/call result per tool.
//
//   - ask_user / await_answers: defensive fallback — these are
//     intercepted (denied or stream-cancelled) by the SDK PreToolUse
//     hooks, so reaching the server means the hook was bypassed.
//   - ask_user_async: the REAL success path. Its hook persists the
//     pending interaction and then ALLOWS the call, so the CLI
//     forwards it here; the canned text tells the model to keep
//     working.
func CallResult(name string, args map[string]any) map[string]any {
	if name == delegate.AskUserAsyncToolName {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": delegate.AsyncQuestionPostedText},
			},
		}
	}
	question, _ := args["question"].(string)
	return map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": fmt.Sprintf("ESCALATION_NOT_INTERCEPTED: %s(%q) was not handled by the iterion runtime. Stop and report this issue.", name, question),
			},
		},
		"isError": true,
	}
}
