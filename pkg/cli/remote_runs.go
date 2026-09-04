package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// remoteRunSummary mirrors runview.RunSummary's CLI-visible fields.
type remoteRunSummary struct {
	ID           string     `json:"id"`
	Name         string     `json:"name,omitempty"`
	WorkflowName string     `json:"workflow_name"`
	BundleName   string     `json:"bundle_name,omitempty"`
	SourceKind   string     `json:"source_kind,omitempty"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// RemoteRunsListOptions filter the run list (server-side query params).
type RemoteRunsListOptions struct {
	Status   string
	Workflow string
	Repo     string
	Since    string // RFC3339, passed through verbatim
	Limit    int
}

func RemoteRunsList(ctx context.Context, c *RemoteClient, p *Printer, opts RemoteRunsListOptions) error {
	q := map[string]string{
		"status":   opts.Status,
		"workflow": opts.Workflow,
		"repo":     opts.Repo,
		"since":    opts.Since,
	}
	if opts.Limit > 0 {
		q["limit"] = fmt.Sprintf("%d", opts.Limit)
	}
	path := "/api/runs" + QueryString(q)
	if p.Format == OutputJSON {
		return RemoteGetPrint(ctx, c, p, path)
	}
	var out struct {
		Runs []remoteRunSummary `json:"runs"`
	}
	if _, err := c.Call(ctx, "GET", path, nil, &out); err != nil {
		return err
	}
	rows := make([][]string, 0, len(out.Runs))
	for _, r := range out.Runs {
		name := r.Name
		if name == "" {
			name = r.WorkflowName
		}
		rows = append(rows, []string{r.ID, name, r.Status, FormatTime(r.CreatedAt)})
	}
	p.Table([]string{"RUN ID", "NAME", "STATUS", "CREATED"}, rows)
	return nil
}

// RemoteRunsLaunchOptions mirrors the POST /api/runs body the CLI fills.
type RemoteRunsLaunchOptions struct {
	FilePath        string // local .bot file; read and sent inline as source
	BotID           string // catalog bot id (alternative to FilePath)
	Vars            map[string]string
	Preset          string
	Timeout         string
	Backend         string
	Compress        string
	AutoMemory      string
	LoopBudgetGuard string
	Permission      string
	ReviewMode      string
	MergeInto       string
	BranchName      string
	MergeStrategy   string
	AutoMerge       bool
	// Attach maps attachment name -> local file path; each is uploaded
	// to /api/runs/uploads first and referenced by upload id.
	Attach             map[string]string
	ModelOverridesJSON []byte // raw model_overrides array (from @file)
	CallbackURL        string
	CallbackToken      string
	Follow             bool
	FollowInterval     time.Duration
}

func RemoteRunsLaunch(ctx context.Context, c *RemoteClient, p *Printer, opts RemoteRunsLaunchOptions) error {
	if opts.FilePath == "" && opts.BotID == "" {
		return fmt.Errorf("a workflow file or --bot <catalog id> is required")
	}
	req := map[string]any{}
	if opts.FilePath != "" {
		src, err := os.ReadFile(opts.FilePath)
		if err != nil {
			return err
		}
		req["source"] = string(src)
		req["file_path"] = opts.FilePath
	}
	if opts.BotID != "" {
		req["bot_id"] = opts.BotID
	}
	if len(opts.Vars) > 0 {
		req["vars"] = opts.Vars
	}
	for k, v := range map[string]string{
		"preset":            opts.Preset,
		"timeout":           opts.Timeout,
		"backend":           opts.Backend,
		"compress":          opts.Compress,
		"auto_memory":       opts.AutoMemory,
		"loop_budget_guard": opts.LoopBudgetGuard,
		"permission":        opts.Permission,
		"review_mode":       opts.ReviewMode,
		"merge_into":        opts.MergeInto,
		"branch_name":       opts.BranchName,
		"merge_strategy":    opts.MergeStrategy,
		"callback_url":      opts.CallbackURL,
		"callback_token":    opts.CallbackToken,
	} {
		if v != "" {
			req[k] = v
		}
	}
	if opts.AutoMerge {
		req["auto_merge"] = true
	}
	if len(opts.ModelOverridesJSON) > 0 {
		var overrides []map[string]any
		if err := json.Unmarshal(opts.ModelOverridesJSON, &overrides); err != nil {
			return fmt.Errorf("parse --model-overrides: %w (want a JSON array)", err)
		}
		req["model_overrides"] = overrides
	}
	if len(opts.Attach) > 0 {
		attachments := make(map[string]string, len(opts.Attach))
		for name, path := range opts.Attach {
			id, err := RemoteRunsUploadFile(ctx, c, path)
			if err != nil {
				return fmt.Errorf("upload attachment %q: %w", name, err)
			}
			attachments[name] = id
		}
		req["attachments"] = attachments
	}

	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	raw, err := c.Call(ctx, "POST", "/api/runs", req, &out)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON && !opts.Follow {
		PrintRemoteJSON(p, raw)
		return nil
	}
	p.Line("Run %s launched (%s)", out.RunID, out.Status)
	if opts.Follow {
		return RemoteRunsFollow(ctx, c, p, out.RunID, opts.FollowInterval)
	}
	return nil
}

// RemoteRunsUploadFile stages a local file via POST /api/runs/uploads
// and returns the upload id.
func RemoteRunsUploadFile(ctx context.Context, c *RemoteClient, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var out struct {
		UploadID string `json:"upload_id"`
	}
	if _, err := c.Upload(ctx, "/api/runs/uploads", "file", filepath.Base(path), f, nil, &out); err != nil {
		return "", err
	}
	if out.UploadID == "" {
		return "", fmt.Errorf("upload succeeded but no upload_id returned")
	}
	return out.UploadID, nil
}

func RemoteRunsGet(ctx context.Context, c *RemoteClient, p *Printer, id string) error {
	var run struct {
		Run struct {
			ID         string     `json:"id"`
			Name       string     `json:"name"`
			Status     string     `json:"status"`
			FilePath   string     `json:"file_path"`
			CreatedAt  time.Time  `json:"created_at"`
			FinishedAt *time.Time `json:"finished_at"`
			Error      string     `json:"error"`
		} `json:"run"`
	}
	raw, err := c.Call(ctx, "GET", "/api/runs/"+id, nil, &run)
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		PrintRemoteJSON(p, raw)
		return nil
	}
	// Unrecognized/empty decode → lossless raw passthrough.
	if run.Run.ID == "" {
		PrintRemoteJSON(p, raw)
		return nil
	}
	p.KV("Run", run.Run.ID)
	if run.Run.Name != "" {
		p.KV("Name", run.Run.Name)
	}
	p.KV("Status", run.Run.Status)
	if run.Run.FilePath != "" {
		p.KV("Workflow", run.Run.FilePath)
	}
	p.KV("Created", FormatTime(run.Run.CreatedAt))
	if run.Run.FinishedAt != nil {
		p.KV("Finished", FormatTime(*run.Run.FinishedAt))
	}
	if run.Run.Error != "" {
		p.KV("Error", run.Run.Error)
	}
	return nil
}

type remoteEvent struct {
	Seq       int64          `json:"seq"`
	Timestamp time.Time      `json:"timestamp"`
	Type      string         `json:"type"`
	NodeID    string         `json:"node_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// RemoteRunsEvents prints a page of events; with follow it keeps
// polling by seq cursor until the run reaches a terminal status.
func RemoteRunsEvents(ctx context.Context, c *RemoteClient, p *Printer, id string, from int64, follow bool, interval time.Duration) error {
	if !follow {
		var out struct {
			Events []remoteEvent `json:"events"`
		}
		raw, err := c.Call(ctx, "GET", fmt.Sprintf("/api/runs/%s/events?from=%d", id, from), nil, &out)
		if err != nil {
			return err
		}
		if p.Format == OutputJSON {
			PrintRemoteJSON(p, raw)
			return nil
		}
		for _, e := range out.Events {
			printRemoteEvent(p, e)
		}
		return nil
	}
	_, err := followRemoteRun(ctx, c, p, id, from, interval)
	return err
}

// RemoteRunsFollow tails a run until it terminates; exits with an error
// when the terminal status is failed/failed_resumable/cancelled.
func RemoteRunsFollow(ctx context.Context, c *RemoteClient, p *Printer, id string, interval time.Duration) error {
	status, err := followRemoteRun(ctx, c, p, id, 0, interval)
	if err != nil {
		return err
	}
	switch status {
	case "finished":
		p.Line("Run %s finished", id)
		return nil
	default:
		return fmt.Errorf("run %s ended with status %s", id, status)
	}
}

// followNotFoundGrace bounds how long follow tolerates 404s on a run
// that was just launched: the POST /api/runs response can precede the
// store's first run.json write, so the run is briefly invisible. Past
// the grace window the 404 is a real error and surfaces.
const followNotFoundGrace = 30 * time.Second

// followRemoteRun polls the seq-cursor events endpoint, printing events
// as they arrive, and returns the run's terminal status.
func followRemoteRun(ctx context.Context, c *RemoteClient, p *Printer, id string, from int64, interval time.Duration) (string, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	cursor := from
	var notFoundSince time.Time
	tolerate404 := func(err error) (bool, error) {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 404 {
			return false, err
		}
		if notFoundSince.IsZero() {
			notFoundSince = time.Now()
		}
		if time.Since(notFoundSince) > followNotFoundGrace {
			return false, fmt.Errorf("run %s still not visible after %s: %w", id, followNotFoundGrace, err)
		}
		return true, nil
	}
	wait := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
			return nil
		}
	}
	for {
		var out struct {
			Events []remoteEvent `json:"events"`
		}
		if _, err := c.Call(ctx, "GET", fmt.Sprintf("/api/runs/%s/events?from=%d", id, cursor), nil, &out); err != nil {
			ok, err := tolerate404(err)
			if !ok {
				return "", err
			}
			if err := wait(); err != nil {
				return "", err
			}
			continue
		}
		advanced := false
		for _, e := range out.Events {
			if p.Format == OutputJSON {
				p.JSON(e)
			} else {
				printRemoteEvent(p, e)
			}
			if e.Seq >= cursor {
				cursor = e.Seq + 1
				advanced = true
			}
		}
		// New events mean more may already be waiting — poll again right
		// away. Gated on the cursor actually moving so a page of
		// already-seen events can't turn the loop into a zero-delay spin.
		if advanced {
			continue
		}
		var snap struct {
			Run struct {
				Status string `json:"status"`
			} `json:"run"`
		}
		if _, err := c.Call(ctx, "GET", "/api/runs/"+id, nil, &snap); err != nil {
			ok, err := tolerate404(err)
			if !ok {
				return "", err
			}
			if err := wait(); err != nil {
				return "", err
			}
			continue
		}
		notFoundSince = time.Time{}
		switch snap.Run.Status {
		case "finished", "failed", "failed_resumable", "cancelled":
			return snap.Run.Status, nil
		}
		if err := wait(); err != nil {
			return "", err
		}
	}
}

func printRemoteEvent(p *Printer, e remoteEvent) {
	node := e.NodeID
	if node != "" {
		node = " " + node
	}
	detail := ""
	if msg, ok := e.Data["message"].(string); ok && msg != "" {
		detail = " — " + firstLine([]byte(msg))
	}
	p.Line("%s  %-22s%s%s", e.Timestamp.Format("15:04:05"), e.Type, node, detail)
}

// RemoteRunsRaw streams a raw endpoint (log, workflow source) to output.
func RemoteRunsRaw(ctx context.Context, c *RemoteClient, p *Printer, id, endpoint string) error {
	code, body, err := c.API(ctx, "GET", "/api/runs/"+id+endpoint, nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return &APIError{Status: code, Method: "GET", Path: "/api/runs/" + id + endpoint, Body: string(body)}
	}
	p.Line("%s", strings.TrimRight(string(body), "\n"))
	return nil
}

// RemoteRunsArtifacts lists artifacts, or the artifact file tree/content.
func RemoteRunsArtifacts(ctx context.Context, c *RemoteClient, p *Printer, id, node, file string) error {
	path := "/api/runs/" + id + "/artifacts"
	if node != "" {
		path += "/" + node
	}
	if file != "" {
		path = "/api/runs/" + id + "/artifact-files/" + strings.TrimPrefix(file, "/")
	}
	raw, err := c.Call(ctx, "GET", path, nil, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemoteRunsFilesOptions selects the workspace-files view.
type RemoteRunsFilesOptions struct {
	Path     string
	Mode     string // "uncommitted" | "branch" (server-side default applies)
	Diff     bool
	Content  bool
	EditFile string // local file whose bytes replace the remote path (PUT)
}

func RemoteRunsFiles(ctx context.Context, c *RemoteClient, p *Printer, id string, opts RemoteRunsFilesOptions) error {
	switch {
	case opts.EditFile != "":
		if opts.Path == "" {
			return fmt.Errorf("--edit requires a target path argument")
		}
		b, err := os.ReadFile(opts.EditFile)
		if err != nil {
			return err
		}
		body := map[string]string{"path": opts.Path, "content": string(b)}
		raw, err := c.Call(ctx, "PUT", "/api/runs/"+id+"/files/content", body, nil)
		if err != nil {
			return err
		}
		PrintRemoteJSON(p, raw)
		return nil
	case opts.Diff:
		if opts.Path == "" {
			return fmt.Errorf("--diff requires a path argument")
		}
		q := QueryString(map[string]string{"path": opts.Path, "mode": opts.Mode})
		raw, err := c.Call(ctx, "GET", "/api/runs/"+id+"/files/diff"+q, nil, nil)
		if err != nil {
			return err
		}
		PrintRemoteJSON(p, raw)
		return nil
	case opts.Content:
		if opts.Path == "" {
			return fmt.Errorf("--content requires a path argument")
		}
		raw, err := c.Call(ctx, "GET", "/api/runs/"+id+"/files/content"+QueryString(map[string]string{"path": opts.Path}), nil, nil)
		if err != nil {
			return err
		}
		PrintRemoteJSON(p, raw)
		return nil
	default:
		raw, err := c.Call(ctx, "GET", "/api/runs/"+id+"/files"+QueryString(map[string]string{"mode": opts.Mode}), nil, nil)
		if err != nil {
			return err
		}
		PrintRemoteJSON(p, raw)
		return nil
	}
}

func RemoteRunsCommits(ctx context.Context, c *RemoteClient, p *Printer, id, sha string, diff bool) error {
	path := "/api/runs/" + id + "/commits"
	if sha != "" {
		path += "/" + sha
		if diff {
			path += "/diff"
		}
	}
	raw, err := c.Call(ctx, "GET", path, nil, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemoteRunsAction posts a bodyless (or fixed-body) lifecycle action.
func RemoteRunsAction(ctx context.Context, c *RemoteClient, p *Printer, id, action string, body any) error {
	raw, err := c.Call(ctx, "POST", "/api/runs/"+id+"/"+action, body, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}

// RemoteRunsResumeOptions mirrors resumeRunRequest.
type RemoteRunsResumeOptions struct {
	AnswersFile string // JSON map of answers (@file semantics handled by caller)
	FilePath    string // optionally push a modified workflow
	Force       bool
	Timeout     string
	// Budget overrides: non-zero fields beat the run doc's persisted
	// launch ask on THIS resume — the "raise the cap + resume"
	// recovery the local CLI has (`iterion resume --max-*`), extended
	// to the remote path per #652 part 2. Zero fields inherit.
	MaxCostUSD          float64
	MaxTokens           int
	MaxDuration         string
	MaxIterations       int
	MaxParallelBranches int
}

func RemoteRunsResume(ctx context.Context, c *RemoteClient, p *Printer, id string, opts RemoteRunsResumeOptions) error {
	req := map[string]any{}
	if opts.AnswersFile != "" {
		b, err := os.ReadFile(opts.AnswersFile)
		if err != nil {
			return err
		}
		var answers map[string]any
		if err := json.Unmarshal(b, &answers); err != nil {
			return fmt.Errorf("parse answers file: %w (want a JSON object)", err)
		}
		req["answers"] = answers
	}
	if opts.FilePath != "" {
		src, err := os.ReadFile(opts.FilePath)
		if err != nil {
			return err
		}
		req["source"] = string(src)
		req["file_path"] = opts.FilePath
	}
	if opts.Force {
		req["force"] = true
	}
	if opts.Timeout != "" {
		req["timeout"] = opts.Timeout
	}
	if budget := resumeBudgetBody(opts); budget != nil {
		// E3 (#652 review round 1): validate client-side so a typo
		// (--max-duration "4 hours") fails immediately with an
		// actionable message, before the round trip. The server
		// re-validates as the authoritative gate.
		if err := (ir.BudgetOverrides{
			MaxCostUSD:          opts.MaxCostUSD,
			MaxTokens:           opts.MaxTokens,
			MaxDuration:         opts.MaxDuration,
			MaxIterations:       opts.MaxIterations,
			MaxParallelBranches: opts.MaxParallelBranches,
		}).Validate(); err != nil {
			return fmt.Errorf("invalid budget: %w", err)
		}
		req["budget"] = budget
	}
	return RemoteRunsAction(ctx, c, p, id, "resume", req)
}

// resumeBudgetBody projects the non-zero budget fields onto the wire
// object, or returns nil when every field is zero so an ask-less resume
// stays byte-identical to the pre-#652 payload (older servers ignore
// the unknown "budget" field, but a nil is cleaner).
func resumeBudgetBody(opts RemoteRunsResumeOptions) map[string]any {
	body := map[string]any{}
	if opts.MaxCostUSD > 0 {
		body["max_cost_usd"] = opts.MaxCostUSD
	}
	if opts.MaxTokens > 0 {
		body["max_tokens"] = opts.MaxTokens
	}
	if opts.MaxDuration != "" {
		body["max_duration"] = opts.MaxDuration
	}
	if opts.MaxIterations > 0 {
		body["max_iterations"] = opts.MaxIterations
	}
	if opts.MaxParallelBranches > 0 {
		body["max_parallel_branches"] = opts.MaxParallelBranches
	}
	if len(body) == 0 {
		return nil
	}
	return body
}

func RemoteRunsDelete(ctx context.Context, c *RemoteClient, p *Printer, id string) error {
	raw, err := c.Call(ctx, "DELETE", "/api/runs/"+id, nil, nil)
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		PrintRemoteJSON(p, raw)
	} else {
		p.Line("Run %s deleted", id)
	}
	return nil
}

func RemoteRunsPreviewCost(ctx context.Context, c *RemoteClient, p *Printer, filePath string, vars map[string]string) error {
	src, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	req := map[string]any{"source": string(src), "file_path": filePath}
	if len(vars) > 0 {
		req["vars"] = vars
	}
	raw, err := c.Call(ctx, "POST", "/api/runs/preview-cost", req, nil)
	if err != nil {
		return err
	}
	PrintRemoteJSON(p, raw)
	return nil
}
