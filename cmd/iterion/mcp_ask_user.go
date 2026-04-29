package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/SocialGouv/iterion/pkg/cli"
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
var askUserInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "The clarifying question to ask the human user."
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`)

type mcpRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *mcpError        `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// runMCPAskUserServer runs a line-delimited JSON-RPC loop on the given streams.
// It returns nil on clean EOF.
func runMCPAskUserServer(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// MCP messages can exceed the default 64KB buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// Per JSON-RPC: on parse error with no recoverable id, reply with id=null.
			_ = enc.Encode(mcpResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &mcpError{Code: -32700, Message: fmt.Sprintf("parse error: %s", err)},
			})
			continue
		}

		// Notifications (no id) get no response.
		if req.ID == nil {
			continue
		}

		resp := dispatchMCPAskUser(req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func dispatchMCPAskUser(req mcpRequest) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "iterion-ask-user",
				"version": cli.Version(),
			},
		}
	case "tools/list":
		resp.Result = map[string]any{
			"tools": []map[string]any{
				{
					"name":        askUserToolName,
					"description": "Pause execution and ask the human running this workflow a clarifying question. Use this when you need information, approval, or guidance you cannot derive yourself.",
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
			resp.Error = &mcpError{Code: -32602, Message: fmt.Sprintf("invalid params: %s", err)}
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
		resp.Error = &mcpError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}
	return resp
}
