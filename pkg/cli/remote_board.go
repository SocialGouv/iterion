package cli

import (
	"context"
	"strings"
)

// remoteIssue mirrors the native tracker's Issue wire shape.
type remoteIssue struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	State    string   `json:"state"`
	Labels   []string `json:"labels,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Assignee string   `json:"assignee,omitempty"`
	Bot      string   `json:"bot,omitempty"`
}

// RemoteIssuesListOptions filter the board issue list.
type RemoteIssuesListOptions struct {
	States   []string
	Labels   []string
	Assignee string
}

func RemoteIssuesList(ctx context.Context, c *RemoteClient, p *Printer, opts RemoteIssuesListOptions) error {
	q := make([]string, 0, 4)
	for _, s := range opts.States {
		q = append(q, "state="+s)
	}
	for _, l := range opts.Labels {
		q = append(q, "label="+l)
	}
	if opts.Assignee != "" {
		q = append(q, "assignee="+opts.Assignee)
	}
	path := "/api/v1/native/issues"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	if p.Format == OutputJSON {
		return RemoteGetPrint(ctx, c, p, path)
	}
	var issues []remoteIssue
	if _, err := c.Call(ctx, "GET", path, nil, &issues); err != nil {
		return err
	}
	rows := make([][]string, 0, len(issues))
	for _, i := range issues {
		rows = append(rows, []string{i.ID, i.Title, i.State, strings.Join(i.Labels, ","), i.Assignee, i.Bot})
	}
	p.Table([]string{"ID", "TITLE", "STATE", "LABELS", "ASSIGNEE", "BOT"}, rows)
	return nil
}

// RemoteIssueFields carries the create/update payload the CLI exposes.
type RemoteIssueFields struct {
	Title    string
	Body     string
	State    string
	Labels   []string
	Priority int
	Assignee string
	Bot      string
	BotArgs  map[string]string
}

func RemoteIssuesCreate(ctx context.Context, c *RemoteClient, p *Printer, f RemoteIssueFields) error {
	req := map[string]any{"title": f.Title}
	if f.Body != "" {
		req["body"] = f.Body
	}
	if f.State != "" {
		req["state"] = f.State
	}
	if len(f.Labels) > 0 {
		req["labels"] = f.Labels
	}
	if f.Priority != 0 {
		req["priority"] = f.Priority
	}
	if f.Assignee != "" {
		req["assignee"] = f.Assignee
	}
	if f.Bot != "" {
		req["bot"] = f.Bot
	}
	if len(f.BotArgs) > 0 {
		req["bot_args"] = f.BotArgs
	}
	raw, err := c.Call(ctx, "POST", "/api/v1/native/issues", req, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemoteIssuesUpdate PATCHes only the explicitly-set fields (set maps
// flag-name presence, so an empty string can clear a field).
func RemoteIssuesUpdate(ctx context.Context, c *RemoteClient, p *Printer, id string, f RemoteIssueFields, set map[string]bool) error {
	req := map[string]any{}
	if set["title"] {
		req["title"] = f.Title
	}
	if set["body"] {
		req["body"] = f.Body
	}
	if set["label"] {
		req["labels"] = f.Labels
	}
	if set["priority"] {
		req["priority"] = f.Priority
	}
	if set["assignee"] {
		req["assignee"] = f.Assignee
	}
	if set["bot"] {
		req["bot"] = f.Bot
	}
	if set["bot-arg"] {
		req["bot_args"] = f.BotArgs
	}
	raw, err := c.Call(ctx, "PATCH", "/api/v1/native/issues/"+id, req, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}
