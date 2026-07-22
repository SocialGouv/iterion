package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/SocialGouv/iterion/pkg/askusermcp"
)

// mcpAskUserCmd runs a minimal MCP stdio server that exposes the ask-user
// tool set (ask_user, ask_user_async, await_answers) advertised to the claude
// CLI subprocess. The claude_code delegate registers this server (via
// os.Executable() + this subcommand) so the LLM has a native tool to call when
// it needs human input. iterion intercepts the calls at the SDK PreToolUse
// hook level — this server's tools/call handler is a defensive fallback in
// case a hook is bypassed (and the canned success path for ask_user_async).
// The tool descriptors + call results live in pkg/askusermcp, shared with the
// HTTP transport the engine binds for sandboxed runs (ADR-082 Phase 3).
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
		tools := askusermcp.Tools()
		entries := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			entries = append(entries, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		resp.Result = map[string]any{"tools": entries}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = mcpInvalidParamsError(err)
			return resp
		}
		resp.Result = askusermcp.CallResult(params.Name, params.Arguments)
	default:
		resp.Error = mcpMethodNotFoundError(req.Method)
	}
	return resp
}
