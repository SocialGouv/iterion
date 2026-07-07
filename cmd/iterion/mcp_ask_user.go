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

const askUserToolName = "ask_user"

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
		// Defensive fallback: this handler should not be reached in practice because
		// iterion intercepts ask_user at the SDK PreToolUse hook level. If we get here,
		// the hook was bypassed — return a tool_result that tells the LLM to stop and
		// flag the situation.
		question, _ := params.Arguments["question"].(string)
		resp.Result = map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": fmt.Sprintf("ESCALATION_NOT_INTERCEPTED: ask_user(%q) was not handled by the iterion runtime. Stop and report this issue.", question),
				},
			},
			"isError": true,
		}
	default:
		resp.Error = mcpMethodNotFoundError(req.Method)
	}
	return resp
}
