package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// mcpAskUserCmd runs a minimal MCP stdio server that exposes a single tool,
// `ask_user`, advertised to the claude CLI subprocess. The claude_code delegate
// registers this server (via os.Executable() + this subcommand) so the LLM has
// a native tool to call when it needs human input. iterion intercepts the call
// at the SDK PreToolUse hook level — this server's tools/call handler is a
// defensive fallback in case the hook is bypassed.
//
// The "__" prefix marks this as an internal subcommand: not user-facing and not
// listed in help output.
var mcpAskUserCmd = &cobra.Command{
	Use:    "__mcp-ask-user",
	Short:  "Internal: MCP stdio server exposing the ask_user tool",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMCPAskUserServer(os.Stdin, os.Stdout)
	},
}

func init() {
	rootCmd.AddCommand(mcpAskUserCmd)
}

const (
	askUserToolName      = "ask_user"
	askUserAsyncToolName = "ask_user_async"
	awaitAnswersToolName = "await_answers"
)

// askUserInputSchema is the JSON Schema for the ask_user tool input.
// Mirrors claw-code-go's native ask_user tool shape (options + free
// text) so both backends offer the LLM the same structured contract.
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

// awaitAnswersInputSchema is the JSON Schema for the await_answers tool
// input (an optional note; the real state lives in the run's interaction
// store, consulted by the PreToolUse hook).
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

// mcpAskUserCallResult builds the tools/call result per tool.
//
//   - ask_user / await_answers: defensive fallback — these are intercepted
//     (denied or stream-cancelled) by the SDK PreToolUse hooks, so reaching
//     the server means the hook was bypassed.
//   - ask_user_async: the REAL success path. Its hook persists the pending
//     interaction and then ALLOWS the call, so the CLI forwards it here;
//     the canned text tells the model to keep working.
func mcpAskUserCallResult(name string, args map[string]any) map[string]any {
	if name == askUserAsyncToolName {
		return map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": "Question posted. The operator's answer will arrive in your conversation as an operator message tagged with the question id — keep working on everything that does not depend on it. Call await_answers only when you cannot proceed without the pending answers.",
				},
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

// runMCPAskUserServer runs a line-delimited JSON-RPC loop on the given streams.
// It returns nil on clean EOF. MCP messages can exceed the 64KB default
// buffer, so the loop is sized at 1MB.
func runMCPAskUserServer(in io.Reader, out io.Writer) error {
	return runMCPLoop(in, out, 1024*1024, dispatchMCPAskUser)
}

func dispatchMCPAskUser(req mcpRequest) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = mcpInitializeResult("iterion-ask-user")
	case "tools/list":
		resp.Result = map[string]any{
			"tools": []map[string]any{
				{
					"name":        askUserToolName,
					"description": "Pause execution and ask the human running this workflow a clarifying question. Use this when you need information, approval, or guidance you cannot derive yourself. Optional `options` present the user with clickable choices; when `allow_free_text` is true the user may also type a free response.",
					"inputSchema": askUserInputSchema,
				},
				{
					"name":        askUserAsyncToolName,
					"description": "Ask the human operator a question WITHOUT pausing your work. The question is posted immediately and you keep working; the operator's answer arrives later in your conversation as an operator message tagged with the question id. Post questions as early as possible. Use ask_user instead when you cannot take another step without the answer.",
					"inputSchema": askUserInputSchema,
				},
				{
					"name":        awaitAnswersToolName,
					"description": "Wait for the operator's answers to the questions you posted with ask_user_async. Call this ONLY when you truly cannot proceed without the pending answers. If everything is already answered it returns the answers immediately; otherwise the run pauses until the operator replies.",
					"inputSchema": awaitAnswersInputSchema,
				},
			},
		}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = mcpInvalidParamsError(err)
			return resp
		}
		resp.Result = mcpAskUserCallResult(params.Name, params.Arguments)
	default:
		resp.Error = mcpMethodNotFoundError(req.Method)
	}
	return resp
}
