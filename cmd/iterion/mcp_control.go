package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// mcpControlCmd runs a stdio MCP server that exposes the running
// iterion-desktop's HTTP API to a parent claude CLI. Useful when the
// operator wants to drive the desktop autonomously from a separate
// claude session (e.g. an agent running inside a devcontainer that
// can't reach the loopback port directly): the discovery file written
// by the desktop on every server start (cmd/iterion-desktop/url_file.go)
// is the bridge.
//
// Tools exposed:
//   - desktop_status   — discovery file contents + reachability check
//   - launch_run       — POST /api/runs (start a new run)
//   - get_run          — GET /api/runs/{id}
//   - get_run_log      — GET /api/runs/{id}/log (with optional tail)
//   - get_run_events   — GET /api/runs/{id}/events (with optional since)
//   - list_runs        — GET /api/runs (with filters)
//   - cancel_run       — POST /api/runs/{id}/cancel
//
// All tools return the raw JSON/text from the API as a tool_result text
// block — the LLM parses what it needs. The "__" prefix marks this as
// internal: not user-facing, hidden from help output.
var mcpControlCmd = &cobra.Command{
	Use:    "__mcp-control",
	Short:  "Internal: MCP stdio server exposing the running desktop's HTTP API",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMCPControlServer(os.Stdin, os.Stdout)
	},
}

func init() {
	rootCmd.AddCommand(mcpControlCmd)
}

// controlClient resolves the desktop URL from the discovery file on
// every call so a desktop restart (new ephemeral port) is picked up
// without restarting the MCP server.
type controlClient struct {
	hc *http.Client
}

func newControlClient() *controlClient {
	return &controlClient{
		hc: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *controlClient) baseURL() (string, error) {
	dir := iterionDataDir()
	if dir == "" {
		return "", fmt.Errorf("could not resolve iterion data dir (set $ITERION_HOME or $HOME)")
	}
	path := filepath.Join(dir, "desktop.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w (is iterion-desktop running?)", path, err)
	}
	var state struct {
		URL string `json:"url"`
		PID int    `json:"pid"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if state.URL == "" {
		return "", fmt.Errorf("%s has empty url field (desktop start failed?)", path)
	}
	return strings.TrimRight(state.URL, "/"), nil
}

func (c *controlClient) get(pathQuery string) (int, []byte, error) {
	base, err := c.baseURL()
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.hc.Get(base + pathQuery)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func (c *controlClient) post(path string, payload any) (int, []byte, error) {
	base, err := c.baseURL()
	if err != nil {
		return 0, nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.hc.Post(base+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

// iterionDataDir mirrors store.globalIterionDataDir without taking the
// dependency. Resolution: $ITERION_HOME → $HOME/.iterion → "".
func iterionDataDir() string {
	if dir := strings.TrimRight(os.Getenv("ITERION_HOME"), string(filepath.Separator)); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".iterion")
	}
	return ""
}

// --- MCP plumbing (shared types live in mcp_common.go) ---
//
// Control accepts larger payloads than ask_user/board (run logs can be
// several MB), so the read buffer is sized at 4MB.

func runMCPControlServer(in io.Reader, out io.Writer) error {
	client := newControlClient()
	return runMCPLoop(in, out, 4*1024*1024, func(req mcpRequest) mcpResponse {
		return dispatchMCPControl(req, client)
	})
}

// --- Tool definitions ---

var (
	desktopStatusSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

	launchRunSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {"type": "string", "description": "Absolute or project-relative path to the .bot workflow."},
    "vars":      {"type": "object", "description": "Workflow vars overrides as a string-keyed map.", "additionalProperties": {"type": "string"}},
    "timeout":   {"type": "string", "description": "Go-style duration cap (e.g. '30m', '2h'). Empty disables."},
    "merge_into":     {"type": "string", "description": "Optional worktree-finalisation merge target branch."},
    "branch_name":    {"type": "string", "description": "Override the storage branch name."},
    "merge_strategy": {"type": "string", "description": "'squash' (default) or 'merge'."},
    "auto_merge":     {"type": "boolean", "description": "When true, engine merges at end of run."}
  },
  "required": ["file_path"],
  "additionalProperties": false
}`)

	getRunSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)

	getRunLogSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":   {"type": "string"},
    "tail": {"type": "integer", "description": "Return only the last N lines (default: full log)."}
  },
  "required": ["id"],
  "additionalProperties": false
}`)

	getRunEventsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":    {"type": "string"},
    "since": {"type": "integer", "description": "Only return events with seq > since."}
  },
  "required": ["id"],
  "additionalProperties": false
}`)

	listRunsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "workflow": {"type": "string"},
    "status":   {"type": "string", "description": "One of: pending, running, paused_waiting_human, succeeded, failed, failed_resumable, cancelled."},
    "limit":    {"type": "integer", "description": "Max number of runs to return."}
  },
  "additionalProperties": false
}`)

	cancelRunSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)
)

func dispatchMCPControl(req mcpRequest, client *controlClient) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = mcpInitializeResult("iterion-control")
	case "tools/list":
		resp.Result = map[string]any{
			"tools": []map[string]any{
				{"name": "desktop_status", "description": "Return the discovery-file URL/PID of the running iterion-desktop and ping its /api/server/info endpoint to confirm reachability. Call this first if any other tool fails with a connection error.", "inputSchema": desktopStatusSchema},
				{"name": "launch_run", "description": "Start a new workflow run via POST /api/runs. Returns {run_id, status}. Pair with get_run / get_run_log to follow progress.", "inputSchema": launchRunSchema},
				{"name": "get_run", "description": "Fetch the current run record (status, error, timestamps, workdir). Use this to poll a run's outcome.", "inputSchema": getRunSchema},
				{"name": "get_run_log", "description": "Return the run's plain-text log (run.log). Use 'tail' to limit output for long-running runs.", "inputSchema": getRunLogSchema},
				{"name": "get_run_events", "description": "Return the run's structured event stream (NDJSON parsed to a JSON array). Use 'since' to incrementally tail.", "inputSchema": getRunEventsSchema},
				{"name": "list_runs", "description": "List recent runs with optional filters (workflow, status, limit).", "inputSchema": listRunsSchema},
				{"name": "cancel_run", "description": "Request cancellation of a running workflow.", "inputSchema": cancelRunSchema},
			},
		}
	case "tools/call":
		resp = handleControlToolCall(req, client)
	default:
		resp.Error = mcpMethodNotFoundError(req.Method)
	}
	return resp
}

func handleControlToolCall(req mcpRequest, client *controlClient) mcpResponse {
	resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}

	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		resp.Error = mcpInvalidParamsError(err)
		return resp
	}

	switch params.Name {
	case "desktop_status":
		base, err := client.baseURL()
		if err != nil {
			resp.Result = controlTextResult(fmt.Sprintf("desktop discovery failed: %v", err), true)
			return resp
		}
		status, body, err := client.get("/api/server/info")
		if err != nil {
			resp.Result = controlTextResult(fmt.Sprintf("desktop reachable=false url=%s error=%v", base, err), true)
			return resp
		}
		if status >= 400 {
			resp.Result = controlTextResult(fmt.Sprintf("desktop reachable=true url=%s but /api/server/info returned %d:\n%s", base, status, string(body)), true)
			return resp
		}
		resp.Result = controlTextResult(fmt.Sprintf("desktop reachable=true url=%s\n%s", base, string(body)), false)

	case "launch_run":
		// Pass through the raw args so the server's launchRunRequest
		// schema is the source of truth — keeps this tool aligned with
		// any future fields without re-mapping.
		status, body, err := client.post("/api/runs", params.Arguments)
		resp.Result = controlHTTPResult(status, body, err)

	case "get_run":
		id, _ := params.Arguments["id"].(string)
		if id == "" {
			resp.Result = controlTextResult("missing required argument: id", true)
			return resp
		}
		status, body, err := client.get("/api/runs/" + url.PathEscape(id))
		resp.Result = controlHTTPResult(status, body, err)

	case "get_run_log":
		id, _ := params.Arguments["id"].(string)
		if id == "" {
			resp.Result = controlTextResult("missing required argument: id", true)
			return resp
		}
		status, body, err := client.get("/api/runs/" + url.PathEscape(id) + "/log")
		if err != nil {
			resp.Result = controlTextResult(fmt.Sprintf("http error: %v", err), true)
			return resp
		}
		text := string(body)
		if tail, ok := params.Arguments["tail"].(float64); ok && tail > 0 {
			lines := strings.Split(text, "\n")
			n := int(tail)
			if n < len(lines) {
				lines = lines[len(lines)-n:]
			}
			text = strings.Join(lines, "\n")
		}
		resp.Result = controlTextResult(fmt.Sprintf("HTTP %d\n%s", status, text), status >= 400)

	case "get_run_events":
		id, _ := params.Arguments["id"].(string)
		if id == "" {
			resp.Result = controlTextResult("missing required argument: id", true)
			return resp
		}
		path := "/api/runs/" + url.PathEscape(id) + "/events"
		if since, ok := params.Arguments["since"].(float64); ok && since > 0 {
			path += "?since=" + fmt.Sprintf("%d", int(since))
		}
		status, body, err := client.get(path)
		resp.Result = controlHTTPResult(status, body, err)

	case "list_runs":
		q := url.Values{}
		if v, _ := params.Arguments["workflow"].(string); v != "" {
			q.Set("workflow", v)
		}
		if v, _ := params.Arguments["status"].(string); v != "" {
			q.Set("status", v)
		}
		if v, ok := params.Arguments["limit"].(float64); ok && v > 0 {
			q.Set("limit", fmt.Sprintf("%d", int(v)))
		}
		path := "/api/runs"
		if encoded := q.Encode(); encoded != "" {
			path += "?" + encoded
		}
		status, body, err := client.get(path)
		resp.Result = controlHTTPResult(status, body, err)

	case "cancel_run":
		id, _ := params.Arguments["id"].(string)
		if id == "" {
			resp.Result = controlTextResult("missing required argument: id", true)
			return resp
		}
		status, body, err := client.post("/api/runs/"+url.PathEscape(id)+"/cancel", map[string]any{})
		resp.Result = controlHTTPResult(status, body, err)

	default:
		resp.Error = &mcpError{Code: -32601, Message: fmt.Sprintf("unknown tool: %s", params.Name)}
	}
	return resp
}

// controlTextResult builds a tools/call response with a single text
// content block and the isError flag (which the LLM uses to route the
// result).
func controlTextResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// controlHTTPResult formats an HTTP response (status + body) as a text
// content block. Treats >=400 as isError so the LLM doesn't silently
// accept failed launches.
func controlHTTPResult(status int, body []byte, err error) map[string]any {
	if err != nil {
		return controlTextResult(fmt.Sprintf("http error: %v", err), true)
	}
	return controlTextResult(fmt.Sprintf("HTTP %d\n%s", status, string(body)), status >= 400)
}
