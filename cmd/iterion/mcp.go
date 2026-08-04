package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/SocialGouv/iterion/pkg/operatormcp"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

// mcpCmd runs the operator-facing iterion MCP server on stdio: the
// local_* tools drive this machine's store/engine, the remote_* tools
// drive the logged-in remote instance. Register it in any MCP client,
// e.g. `claude mcp add iterion -- iterion mcp`. See docs/mcp-server.md.
//
// Unlike the hidden __mcp-* servers (internal per-run transports), this
// command is the public surface an operator wires into their agent.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve the iterion MCP server on stdio (local + remote tools)",
	Long: "Serve the operator-facing iterion MCP server over stdio.\n\n" +
		"Two tool families:\n" +
		"  local_*  — this machine's iterion: validate/launch/follow runs,\n" +
		"             the native kanban board, bot discovery. Launches run\n" +
		"             as detached subprocesses that survive the MCP session.\n" +
		"  remote_* — the logged-in remote instance (`iterion remote login`,\n" +
		"             or ITERION_REMOTE_URL + ITERION_REMOTE_TOKEN): typed\n" +
		"             core + the remote_api escape hatch.\n\n" +
		"Register in Claude Code:  claude mcp add iterion -- iterion mcp\n" +
		"Reference: docs/mcp-server.md",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		only, err := operatormcp.ParseFamily(mcpOnly)
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		srv := &operatormcp.Server{
			StoreDir: store.ResolveStoreDir(cwd, mcpStoreDir),
			WorkDir:  cwd,
			ReadOnly: mcpReadOnly,
			Only:     only,
		}
		return runOperatorMCPServer(os.Stdin, os.Stdout, srv)
	},
}

var (
	mcpStoreDir string
	mcpReadOnly bool
	mcpOnly     string
)

func init() {
	mcpCmd.Flags().StringVar(&mcpStoreDir, "store-dir", "", "Run-store directory for the local_* tools (default: standard resolution from the working directory)")
	mcpCmd.Flags().BoolVar(&mcpReadOnly, "read-only", false, "Expose only read tools (remote_api stays available, GET-only)")
	mcpCmd.Flags().StringVar(&mcpOnly, "only", "", "Restrict the tool families: local or remote (default: both)")
	rootCmd.AddCommand(mcpCmd)
}

// runOperatorMCPServer drives the shared line-delimited JSON-RPC loop.
// Buffer sized like __mcp-control: run logs/reports in tool results can
// reach megabytes.
func runOperatorMCPServer(in io.Reader, out io.Writer, srv *operatormcp.Server) error {
	return runMCPLoop(in, out, 4*1024*1024, func(req mcpRequest) mcpResponse {
		return dispatchOperatorMCP(req, srv)
	})
}

func dispatchOperatorMCP(req mcpRequest, srv *operatormcp.Server) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = mcpInitializeResult("iterion")
	case "tools/list":
		tools := srv.Tools()
		entries := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			entries = append(entries, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
				"annotations": map[string]any{"readOnlyHint": t.ReadOnly},
			})
		}
		resp.Result = map[string]any{"tools": entries}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = mcpInvalidParamsError(err)
			return resp
		}
		result, err := srv.Call(context.Background(), params.Name, params.Arguments)
		if err != nil {
			resp.Error = &mcpError{Code: -32601, Message: err.Error()}
			return resp
		}
		resp.Result = result
	default:
		resp.Error = mcpMethodNotFoundError(req.Method)
	}
	return resp
}
