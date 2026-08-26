package operatormcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// remoteTools returns the remote_* tool set: a typed core over the
// high-traffic endpoints plus the remote_api escape hatch and the
// routes/OpenAPI discovery pair — the same "typed core, raw escape
// hatch" positioning as the `iterion remote` CLI.
func remoteTools() []Tool {
	return []Tool{
		{
			Name:        "remote_status",
			Description: "Show the logged-in remote iterion instance, account and active org/team. Call this first to confirm remote connectivity; if it errors, log in with `iterion remote login <url>` (or set ITERION_REMOTE_URL + ITERION_REMOTE_TOKEN).",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			handler:     handleRemoteStatus,
		},
		{
			Name:        "remote_runs_list",
			Description: "List runs on the remote instance with optional filters.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "status":   {"type": "string", "description": "Filter by status (queued, running, paused_waiting_human, finished, failed, failed_resumable, cancelled)."},
    "workflow": {"type": "string"},
    "repo":     {"type": "string", "description": "Filter by target repository slug."},
    "since":    {"type": "string", "description": "Only runs created since this timestamp/duration (server semantics)."},
    "limit":    {"type": "integer"}
  },
  "additionalProperties": false
}`),
			handler: handleRemoteRunsList,
		},
		{
			Name:        "remote_run_get",
			Description: "Fetch one remote run's record (status, error, timestamps, repo).",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"run_id": {"type": "string"}},
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleRemoteRunGet,
		},
		{
			Name:        "remote_run_events",
			Description: "Read a remote run's structured event stream. Use since (last seen seq) to tail incrementally.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_id": {"type": "string"},
    "since":  {"type": "integer", "description": "Only return events with seq >= since."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleRemoteRunEvents,
		},
		{
			Name:        "remote_run_log",
			Description: "Read a remote run's plain-text log. Returns the last `tail` lines (default 200).",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_id": {"type": "string"},
    "tail":   {"type": "integer", "description": "Number of trailing lines to return (default 200)."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleRemoteRunLog,
		},
		{
			Name:        "remote_run_artifacts",
			Description: "List a remote run's artifacts, one node's artifact content, or a produced file (artifact-files) by path.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_id": {"type": "string"},
    "node":   {"type": "string", "description": "Node id — returns that node's latest artifact."},
    "file":   {"type": "string", "description": "Artifact-file path — returns that produced file's content instead."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleRemoteRunArtifacts,
		},
		{
			Name:        "remote_bots_list",
			Description: "List the bot catalog of the remote instance.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			handler: func(ctx context.Context, s *Server, _ json.RawMessage) (string, bool, error) {
				return remoteHTTP(ctx, "GET", "/api/v1/bots", nil)
			},
		},
		{
			Name:        "remote_bots_get",
			Description: "Fetch one remote catalog bot's metadata by id.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"name": {"type": "string", "description": "Catalog bot id (e.g. review-pr)."}},
  "required": ["name"],
  "additionalProperties": false
}`),
			handler: handleRemoteBotsGet,
		},
		{
			Name:        "remote_issues_list",
			Description: "List issues on the remote native kanban board, with optional state/label/assignee filters.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "states":   {"type": "array", "items": {"type": "string"}},
    "labels":   {"type": "array", "items": {"type": "string"}},
    "assignee": {"type": "string"}
  },
  "additionalProperties": false
}`),
			handler: handleRemoteIssuesList,
		},
		{
			Name:        "remote_routes",
			Description: "List the remote instance's live API routes (method + pattern), optionally filtered by substring. The discovery companion of remote_api.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"filter": {"type": "string", "description": "Case-insensitive substring filter on the route pattern."}},
  "additionalProperties": false
}`),
			handler: handleRemoteRoutes,
		},
		{
			Name:        "remote_openapi",
			Description: "Fetch the remote instance's live OpenAPI 3 spec. ALWAYS pass path_prefix (e.g. /api/webhooks) to keep the payload small — the full spec is very large.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"path_prefix": {"type": "string", "description": "Only include paths starting with this prefix."}},
  "additionalProperties": false
}`),
			handler: handleRemoteOpenAPI,
		},
		{
			Name:        "remote_runs_launch",
			Description: "Launch a run on the remote instance, from a catalog bot id or a local .bot file (uploaded inline). Returns {run_id, status}; follow with remote_run_get / remote_run_events.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "bot_id":      {"type": "string", "description": "Catalog bot id to launch (alternative to file_path)."},
    "file_path":   {"type": "string", "description": "Local .bot file whose source is sent inline (alternative to bot_id)."},
    "vars":        {"type": "object", "additionalProperties": {"type": "string"}},
    "preset":      {"type": "string"},
    "backend":     {"type": "string"},
    "timeout":     {"type": "string"},
    "review_mode": {"type": "string", "description": "auto|mono|dual for bots that declare it."},
    "merge_into":  {"type": "string"},
    "branch_name": {"type": "string"}
  },
  "additionalProperties": false
}`),
			handler: handleRemoteRunsLaunch,
		},
		{
			Name:        "remote_runs_resume",
			Description: "Resume a paused / failed_resumable remote run, optionally with answers for pending human questions.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_id":  {"type": "string"},
    "answers": {"type": "object", "description": "Answers keyed by question/field id."},
    "force":   {"type": "boolean", "description": "Allow resume when the workflow source changed."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleRemoteRunsResume,
		},
		{
			Name:        "remote_runs_cancel",
			Description: "Request cancellation of a remote run.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"run_id": {"type": "string"}},
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleRemoteRunsCancel,
		},
		{
			Name:        "remote_issue_create",
			Description: "Create an issue on the remote native kanban board.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title":    {"type": "string"},
    "body":     {"type": "string"},
    "state":    {"type": "string"},
    "labels":   {"type": "array", "items": {"type": "string"}},
    "priority": {"type": "integer"},
    "assignee": {"type": "string"},
    "bot":      {"type": "string", "description": "Bot id the dispatcher should run for this issue."},
    "bot_args": {"type": "object", "additionalProperties": {"type": "string"}}
  },
  "required": ["title"],
  "additionalProperties": false
}`),
			handler: handleRemoteIssueCreate,
		},
		{
			Name:        "remote_issue_update",
			Description: "Update fields on a remote board issue (PATCH: only the provided fields change).",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":       {"type": "string"},
    "title":    {"type": "string"},
    "body":     {"type": "string"},
    "labels":   {"type": "array", "items": {"type": "string"}},
    "priority": {"type": "integer"},
    "assignee": {"type": "string"},
    "bot":      {"type": "string"},
    "bot_args": {"type": "object", "additionalProperties": {"type": "string"}}
  },
  "required": ["id"],
  "additionalProperties": false
}`),
			handler: handleRemoteIssueUpdate,
		},
		{
			Name:        "remote_issue_transition",
			Description: "Move a remote board issue to another state.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"},
    "to": {"type": "string", "description": "Target state."}
  },
  "required": ["id", "to"],
  "additionalProperties": false
}`),
			handler: handleRemoteIssueTransition,
		},
		{
			Name:        "remote_issue_comment",
			Description: "Comment on a remote board issue; optionally set the dispatching bot and/or transition it in the same call.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id":            {"type": "string"},
    "text":          {"type": "string"},
    "bot":           {"type": "string", "description": "Also stamp this bot for dispatch."},
    "transition_to": {"type": "string", "description": "Also move the issue to this state."}
  },
  "required": ["id", "text"],
  "additionalProperties": false
}`),
			handler: handleRemoteIssueComment,
		},
		{
			Name:        "remote_api",
			Description: "Escape hatch: any authenticated request to the remote instance's HTTP API (the MCP twin of `iterion remote api`). Discover endpoints with remote_routes / remote_openapi. CAN MUTATE (POST/PUT/PATCH/DELETE); in read-only mode only GET is allowed. Returns the HTTP status + raw response body.",
			// Not ReadOnly — the readOnlyHint annotation must be truthful
			// about capability (clients gate auto-approval on it). It
			// stays LISTED in read-only mode because its handler
			// enforces GET-only there.
			ListedInReadOnly: true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "method": {"type": "string", "enum": ["GET", "POST", "PUT", "PATCH", "DELETE"]},
    "path":   {"type": "string", "description": "Absolute API path, e.g. /api/admin/orgs."},
    "body":   {"description": "Optional JSON request body (object or array)."}
  },
  "required": ["method", "path"],
  "additionalProperties": false
}`),
			handler: handleRemoteAPI,
		},
	}
}

// remoteHTTP performs one authenticated call against the logged-in
// instance and renders it as a tool result: raw body on 2xx, an
// explicit `HTTP <code>` header + body with isError on failure.
func remoteHTTP(ctx context.Context, method, path string, body []byte) (string, bool, error) {
	c, err := cli.NewRemoteClient()
	if err != nil {
		return "", false, err
	}
	code, resp, err := c.API(ctx, method, path, body)
	if err != nil {
		return "", false, fmt.Errorf("%s %s%s: %w", method, c.BaseURL(), path, err)
	}
	if code/100 != 2 {
		return fmt.Sprintf("HTTP %d %s %s\n%s", code, method, path, resp), true, nil
	}
	if len(resp) == 0 {
		return fmt.Sprintf("OK (HTTP %d)", code), false, nil
	}
	return string(resp), false, nil
}

func handleRemoteStatus(ctx context.Context, _ *Server, _ json.RawMessage) (string, bool, error) {
	c, err := cli.NewRemoteClient()
	if err != nil {
		return "", false, err
	}
	me, err := c.Me(ctx)
	if err != nil {
		return "", false, fmt.Errorf("reach %s: %w", c.BaseURL(), err)
	}
	out, err := marshalText(map[string]any{
		"instance":       c.BaseURL(),
		"email":          me.User.Email,
		"name":           me.User.Name,
		"is_super_admin": me.User.IsSuperAdmin,
		"active_org_id":  me.ActiveOrg,
		"active_team_id": me.ActiveTeam,
		"orgs":           me.Orgs,
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleRemoteRunsList(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Status   string `json:"status"`
		Workflow string `json:"workflow"`
		Repo     string `json:"repo"`
		Since    string `json:"since"`
		Limit    int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	q := map[string]string{
		"status":   args.Status,
		"workflow": args.Workflow,
		"repo":     args.Repo,
		"since":    args.Since,
	}
	if args.Limit > 0 {
		q["limit"] = fmt.Sprintf("%d", args.Limit)
	}
	return remoteHTTP(ctx, "GET", "/api/runs"+cli.QueryString(q), nil)
}

func requireRunID(raw json.RawMessage) (string, json.RawMessage, error) {
	var args map[string]json.RawMessage
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", nil, err
	}
	var id string
	if v, ok := args["run_id"]; ok {
		if err := json.Unmarshal(v, &id); err != nil {
			return "", nil, fmt.Errorf("invalid run_id: %w", err)
		}
	}
	if id == "" {
		return "", nil, fmt.Errorf("run_id is required")
	}
	return url.PathEscape(id), raw, nil
}

func handleRemoteRunGet(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	id, _, err := requireRunID(raw)
	if err != nil {
		return "", false, err
	}
	return remoteHTTP(ctx, "GET", "/api/runs/"+id, nil)
}

func handleRemoteRunEvents(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
		Since int64  `json:"since"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	return remoteHTTP(ctx, "GET", fmt.Sprintf("/api/runs/%s/events?from=%d", url.PathEscape(args.RunID), args.Since), nil)
}

func handleRemoteRunLog(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
		Tail  int    `json:"tail"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	text, isErr, err := remoteHTTP(ctx, "GET", "/api/runs/"+url.PathEscape(args.RunID)+"/log", nil)
	if err != nil || isErr {
		return text, isErr, err
	}
	tail := args.Tail
	if tail <= 0 {
		tail = 200
	}
	lines := strings.Split(text, "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
		return fmt.Sprintf("(truncated: last %d lines)\n%s", tail, strings.Join(lines, "\n")), false, nil
	}
	return text, false, nil
}

func handleRemoteRunArtifacts(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
		Node  string `json:"node"`
		File  string `json:"file"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	if args.Node != "" && args.File != "" {
		return "", false, fmt.Errorf("node and file are mutually exclusive: node reads a node's artifact, file reads a produced artifact-file")
	}
	id := url.PathEscape(args.RunID)
	path := "/api/runs/" + id + "/artifacts"
	if args.Node != "" {
		path += "/" + url.PathEscape(args.Node)
	}
	if args.File != "" {
		// Escape each segment so `?`/`#`/spaces in a file path cannot
		// smuggle a query string or fragment into the request URL.
		segments := strings.Split(strings.TrimPrefix(args.File, "/"), "/")
		for i, seg := range segments {
			segments[i] = url.PathEscape(seg)
		}
		path = "/api/runs/" + id + "/artifact-files/" + strings.Join(segments, "/")
	}
	return remoteHTTP(ctx, "GET", path, nil)
}

func handleRemoteBotsGet(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.Name == "" {
		return "", false, fmt.Errorf("name is required")
	}
	return remoteHTTP(ctx, "GET", "/api/v1/bots/"+url.PathEscape(args.Name), nil)
}

func handleRemoteIssuesList(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		States   []string `json:"states"`
		Labels   []string `json:"labels"`
		Assignee string   `json:"assignee"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	q := url.Values{}
	for _, s := range args.States {
		q.Add("state", s)
	}
	for _, l := range args.Labels {
		q.Add("label", l)
	}
	if args.Assignee != "" {
		q.Set("assignee", args.Assignee)
	}
	path := "/api/v1/native/issues"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return remoteHTTP(ctx, "GET", path, nil)
}

func handleRemoteRoutes(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Filter string `json:"filter"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	text, isErr, err := remoteHTTP(ctx, "GET", "/api/routes", nil)
	if err != nil || isErr || args.Filter == "" {
		return text, isErr, err
	}
	var rr struct {
		Routes []struct {
			Method  string `json:"method"`
			Pattern string `json:"pattern"`
		} `json:"routes"`
	}
	if uerr := json.Unmarshal([]byte(text), &rr); uerr != nil {
		return text, false, nil // unexpected shape — raw passthrough beats a lossy filter
	}
	needle := strings.ToLower(args.Filter)
	filtered := rr.Routes[:0]
	for _, r := range rr.Routes {
		if strings.Contains(strings.ToLower(r.Pattern), needle) {
			filtered = append(filtered, r)
		}
	}
	out, err := marshalText(map[string]any{"routes": filtered, "filter": args.Filter})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleRemoteOpenAPI(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		PathPrefix string `json:"path_prefix"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	text, isErr, err := remoteHTTP(ctx, "GET", "/api/openapi.json", nil)
	if err != nil || isErr || args.PathPrefix == "" {
		return text, isErr, err
	}
	var spec map[string]json.RawMessage
	if uerr := json.Unmarshal([]byte(text), &spec); uerr != nil {
		return text, false, nil
	}
	var paths map[string]json.RawMessage
	if uerr := json.Unmarshal(spec["paths"], &paths); uerr != nil {
		return text, false, nil
	}
	filtered := make(map[string]json.RawMessage, len(paths))
	for p, v := range paths {
		if strings.HasPrefix(p, args.PathPrefix) {
			filtered[p] = v
		}
	}
	view := map[string]any{
		"openapi":     spec["openapi"],
		"info":        spec["info"],
		"paths":       filtered,
		"path_prefix": args.PathPrefix,
		"note":        "components/schemas omitted in filtered view — fetch without path_prefix for the full spec",
	}
	out, err := marshalText(view)
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleRemoteRunsLaunch(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		BotID      string            `json:"bot_id"`
		FilePath   string            `json:"file_path"`
		Vars       map[string]string `json:"vars"`
		Preset     string            `json:"preset"`
		Backend    string            `json:"backend"`
		Timeout    string            `json:"timeout"`
		ReviewMode string            `json:"review_mode"`
		MergeInto  string            `json:"merge_into"`
		BranchName string            `json:"branch_name"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.BotID == "" && args.FilePath == "" {
		return "", false, fmt.Errorf("bot_id or file_path is required")
	}
	c, err := cli.NewRemoteClient()
	if err != nil {
		return "", false, err
	}
	out, err := captureJSON(func(p *cli.Printer) error {
		return cli.RemoteRunsLaunch(ctx, c, p, cli.RemoteRunsLaunchOptions{
			BotID:      args.BotID,
			FilePath:   s.resolvePath(args.FilePath),
			Vars:       args.Vars,
			Preset:     args.Preset,
			Backend:    args.Backend,
			Timeout:    args.Timeout,
			ReviewMode: args.ReviewMode,
			MergeInto:  args.MergeInto,
			BranchName: args.BranchName,
		})
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleRemoteRunsResume(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID   string         `json:"run_id"`
		Answers map[string]any `json:"answers"`
		Force   bool           `json:"force"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	req := map[string]any{}
	if len(args.Answers) > 0 {
		req["answers"] = args.Answers
	}
	if args.Force {
		req["force"] = true
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", false, err
	}
	return remoteHTTP(ctx, "POST", "/api/runs/"+url.PathEscape(args.RunID)+"/resume", body)
}

func handleRemoteRunsCancel(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	id, _, err := requireRunID(raw)
	if err != nil {
		return "", false, err
	}
	return remoteHTTP(ctx, "POST", "/api/runs/"+id+"/cancel", []byte("{}"))
}

func handleRemoteIssueCreate(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Title    string            `json:"title"`
		Body     string            `json:"body"`
		State    string            `json:"state"`
		Labels   []string          `json:"labels"`
		Priority int               `json:"priority"`
		Assignee string            `json:"assignee"`
		Bot      string            `json:"bot"`
		BotArgs  map[string]string `json:"bot_args"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.Title == "" {
		return "", false, fmt.Errorf("title is required")
	}
	req := map[string]any{"title": args.Title}
	if args.Body != "" {
		req["body"] = args.Body
	}
	if args.State != "" {
		req["state"] = args.State
	}
	if len(args.Labels) > 0 {
		req["labels"] = args.Labels
	}
	if args.Priority != 0 {
		req["priority"] = args.Priority
	}
	if args.Assignee != "" {
		req["assignee"] = args.Assignee
	}
	if args.Bot != "" {
		req["bot"] = args.Bot
	}
	if len(args.BotArgs) > 0 {
		req["bot_args"] = args.BotArgs
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", false, err
	}
	return remoteHTTP(ctx, "POST", "/api/v1/native/issues", body)
}

// remoteIssueUpdatableFields is the PATCH allow-list: argument keys
// copied verbatim into the request when present (JSON-presence
// semantics so an empty string can clear a field, like the CLI).
var remoteIssueUpdatableFields = []string{"title", "body", "labels", "priority", "assignee", "bot", "bot_args"}

func handleRemoteIssueUpdate(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := unmarshalArgs(raw, &fields); err != nil {
		return "", false, err
	}
	var id string
	if v, ok := fields["id"]; ok {
		if err := json.Unmarshal(v, &id); err != nil {
			return "", false, fmt.Errorf("invalid id: %w", err)
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("id is required")
	}
	req := map[string]json.RawMessage{}
	for _, k := range remoteIssueUpdatableFields {
		if v, ok := fields[k]; ok {
			req[k] = v
		}
	}
	if len(req) == 0 {
		return "", false, fmt.Errorf("no fields to update (accepted: %s)", strings.Join(remoteIssueUpdatableFields, ", "))
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", false, err
	}
	return remoteHTTP(ctx, "PATCH", "/api/v1/native/issues/"+url.PathEscape(id), body)
}

func handleRemoteIssueTransition(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		ID string `json:"id"`
		To string `json:"to"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.ID == "" || args.To == "" {
		return "", false, fmt.Errorf("id and to are required")
	}
	body, err := json.Marshal(map[string]string{"to": args.To})
	if err != nil {
		return "", false, err
	}
	return remoteHTTP(ctx, "POST", "/api/v1/native/issues/"+url.PathEscape(args.ID)+"/transition", body)
}

func handleRemoteIssueComment(ctx context.Context, _ *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		ID           string `json:"id"`
		Text         string `json:"text"`
		Bot          string `json:"bot"`
		TransitionTo string `json:"transition_to"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.ID == "" || args.Text == "" {
		return "", false, fmt.Errorf("id and text are required")
	}
	req := map[string]any{"body": args.Text}
	if args.Bot != "" {
		req["bot"] = args.Bot
	}
	if args.TransitionTo != "" {
		req["transition_to"] = args.TransitionTo
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", false, err
	}
	return remoteHTTP(ctx, "POST", "/api/v1/native/issues/"+url.PathEscape(args.ID)+"/comments", body)
}

func handleRemoteAPI(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   any    `json:"body"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return "", false, fmt.Errorf("invalid method %q (want GET, POST, PUT, PATCH or DELETE)", args.Method)
	}
	if s.ReadOnly && method != "GET" {
		return "", false, fmt.Errorf("read-only mode: remote_api only allows GET (requested %s %s)", method, args.Path)
	}
	if !strings.HasPrefix(args.Path, "/") {
		return "", false, fmt.Errorf("path must be absolute (got %q)", args.Path)
	}
	var body []byte
	if args.Body != nil {
		b, err := json.Marshal(args.Body)
		if err != nil {
			return "", false, fmt.Errorf("encode body: %w", err)
		}
		body = b
	}
	c, err := cli.NewRemoteClient()
	if err != nil {
		return "", false, err
	}
	code, resp, err := c.API(ctx, method, args.Path, body)
	if err != nil {
		return "", false, fmt.Errorf("%s %s%s: %w", method, c.BaseURL(), args.Path, err)
	}
	// The escape hatch always shows the status line — the caller asked
	// for the raw exchange.
	return fmt.Sprintf("HTTP %d\n%s", code, resp), code >= 400, nil
}
