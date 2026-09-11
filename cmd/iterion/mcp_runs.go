package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

var mcpRunsCmd = &cobra.Command{
	Use:    "__mcp-runs",
	Short:  "Internal: MCP stdio server exposing capability-gated run reads",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		root := strings.TrimSpace(os.Getenv("ITERION_RUN_STORE_DIR"))
		if root == "" {
			return errors.New("ITERION_RUN_STORE_DIR is required")
		}
		rs, err := store.New(root)
		if err != nil {
			return fmt.Errorf("open run store: %w", err)
		}
		return runMCPRunsServer(cmd.Context(), os.Stdin, os.Stdout, rs, runops.NewCapabilities(os.Getenv("ITERION_RUN_CAPS")))
	},
}

func init() {
	rootCmd.AddCommand(mcpRunsCmd)
}

func runMCPRunsServer(ctx context.Context, in io.Reader, out io.Writer, rs store.RunStore, caps runops.Capabilities) error {
	return runMCPLoop(in, out, 1024*1024, func(req mcpRequest) mcpResponse {
		return dispatchMCPRuns(ctx, req, rs, caps)
	})
}

func dispatchMCPRuns(ctx context.Context, req mcpRequest, rs store.RunStore, caps runops.Capabilities) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = mcpInitializeResult("iterion-runs")
	case "tools/list":
		definitions := runops.ToolsFor(caps)
		entries := make([]map[string]any, 0, len(definitions))
		for _, definition := range definitions {
			entries = append(entries, map[string]any{
				"name": definition.Name, "description": definition.Description, "inputSchema": definition.InputSchema,
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
		raw, err := runops.Call(ctx, rs, caps, params.Name, params.Arguments)
		if err != nil {
			if errors.Is(err, runops.ErrCapabilityDenied) {
				resp.Error = &mcpError{Code: -32601, Message: err.Error()}
				return resp
			}
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
			return resp
		}
		resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}, "isError": false}
	default:
		resp.Error = mcpMethodNotFoundError(req.Method)
	}
	return resp
}
