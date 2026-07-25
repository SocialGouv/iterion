package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/spf13/cobra"
)

// mcpBoardCmd runs the internal MCP stdio server that exposes capability-gated
// access to the native kanban board. claude_code and (in-process) claw bots
// invoke this server when they need to create/move/assign issues from inside a
// running workflow.
//
// Configuration via environment:
//   - ITERION_STORE_DIR : path to the dispatcher store root. If unset, falls
//     back to ./.iterion/dispatcher relative to the current working directory.
//   - ITERION_BOARD_CAPS: comma-separated list of granted capabilities.
//     Empty/unset = no tools exposed (tools/list returns []).
var mcpBoardCmd = &cobra.Command{
	Use:    "__mcp-board",
	Short:  "Internal: MCP stdio server exposing capability-gated board operations",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := openBoardStoreFromEnv()
		if err != nil {
			return err
		}
		caps := boardops.NewCapabilities(os.Getenv("ITERION_BOARD_CAPS"))
		// Planner provenance: the runtime stamps the source ticket so
		// create_issue can auto-set parent_id when the agent omits it.
		env := boardops.CallEnv{SpawnParentID: strings.TrimSpace(os.Getenv("ITERION_SOURCE_ISSUE_ID"))}
		return runMCPBoardServer(os.Stdin, os.Stdout, store, caps, env)
	},
}

func init() {
	rootCmd.AddCommand(mcpBoardCmd)
}

// openBoardStoreFromEnv resolves the dispatcher store root and opens it.
// native.NewStore owns directory creation (it MkdirAlls root + issues/
// itself), so a standalone `iterion run` hitting a fresh workspace
// gets a board lazily without any prior `iterion issue board init`.
func openBoardStoreFromEnv() (*native.Store, error) {
	root := os.Getenv("ITERION_STORE_DIR")
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve cwd: %w", err)
		}
		root = filepath.Join(cwd, ".iterion", "dispatcher")
	}
	return native.NewStore(root)
}

func runMCPBoardServer(in io.Reader, out io.Writer, store *native.Store, caps boardops.Capabilities, env boardops.CallEnv) error {
	return runMCPLoop(in, out, 1024*1024, func(req mcpRequest) mcpResponse {
		return dispatchMCPBoard(req, store, caps, env)
	})
}

func dispatchMCPBoard(req mcpRequest, store *native.Store, caps boardops.Capabilities, env boardops.CallEnv) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = mcpInitializeResult("iterion-board")
	case "tools/list":
		tools := boardops.ToolsFor(caps)
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
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = mcpInvalidParamsError(err)
			return resp
		}
		raw, err := boardops.CallWithEnv(store, caps, params.Name, params.Arguments, env)
		if err != nil {
			if errors.Is(err, boardops.ErrCapabilityDenied) {
				resp.Error = &mcpError{Code: -32601, Message: err.Error()}
				return resp
			}
			// Other errors (unknown state, validation, etc) are returned as
			// tool errors (isError:true) so the LLM can recover.
			resp.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return resp
		}
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(raw)}},
			"isError": false,
		}
	default:
		resp.Error = mcpMethodNotFoundError(req.Method)
	}
	return resp
}
