package operatormcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// localTools returns the local_* tool set (board tools excluded — see
// tools_local_board.go).
func localTools() []Tool {
	return []Tool{
		{
			Name:        "local_validate",
			Description: "Parse, compile and validate a local .bot workflow (or .botz bundle). Returns the validation result JSON including diagnostics; valid:false is a normal outcome, not a tool error.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {"type": "string", "description": "Path to the .bot file or .botz bundle (relative paths resolve against the server's working directory)."}
  },
  "required": ["file_path"],
  "additionalProperties": false
}`),
			handler: handleLocalValidate,
		},
		{
			Name:        "local_bots_list",
			Description: "Discover bots (.bot files and .botz bundles) under the given paths. Default paths: bots, examples (the `iterion bots list` defaults); missing directories are skipped.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "paths": {"type": "array", "items": {"type": "string"}, "description": "Directories or .bot files to scan."}
  },
  "additionalProperties": false
}`),
			handler: handleLocalBotsList,
		},
		{
			Name:        "local_runs_list",
			Description: "List runs in the local store, newest first. Optional status/workflow filters.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "status":   {"type": "string", "description": "Filter by status: queued, running, paused_waiting_human, paused_operator, finished, failed, failed_resumable, cancelled."},
    "workflow": {"type": "string", "description": "Filter by workflow name (exact match)."},
    "limit":    {"type": "integer", "description": "Max runs to return (default 20, max 200)."}
  },
  "additionalProperties": false
}`),
			handler: handleLocalRunsList,
		},
		{
			Name:        "local_run_get",
			Description: "Fetch one local run's record: status, error, budget, worktree finalization (final_commit/final_branch/merged_into), and for detached runs whether the runner process is still alive. Poll this to follow a run launched with local_run.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"run_id": {"type": "string"}},
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleLocalRunGet,
		},
		{
			Name:        "local_run_events",
			Description: "Read a local run's structured event stream (events.jsonl). Use since (last seen seq) to tail incrementally.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_id": {"type": "string"},
    "since":  {"type": "integer", "description": "Only return events with seq >= since."},
    "limit":  {"type": "integer", "description": "Max events to return (default 100, max 1000)."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleLocalRunEvents,
		},
		{
			Name:        "local_run_log",
			Description: "Read a local run's plain-text log (run.log). Returns the last `tail` lines (default 200).",
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
			handler: handleLocalRunLog,
		},
		{
			Name:        "local_run_report",
			Description: "Generate the chronological markdown report for a local run (same as `iterion report`). The best single call to understand what a finished run did.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"run_id": {"type": "string"}},
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleLocalRunReport,
		},
		{
			Name:        "local_questions",
			Description: "List a local run's pending async questions (ask_user_async). Answer them with local_answer.",
			ReadOnly:    true,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"run_id": {"type": "string"}},
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleLocalQuestions,
		},
		{
			Name:        "local_run",
			Description: "Launch a local workflow run. The workflow is validated synchronously, then executed by a DETACHED `iterion run` subprocess that survives this MCP session; the tool returns the run_id immediately. Follow with local_run_get / local_run_events; the run is also visible in a studio bound to the same store.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path":   {"type": "string", "description": "Path to the .bot file or .botz bundle to run."},
    "vars":        {"type": "object", "description": "Workflow vars overrides.", "additionalProperties": {"type": "string"}},
    "timeout":     {"type": "string", "description": "Go-style duration cap for the run (e.g. '30m', '2h')."},
    "merge_into":  {"type": "string", "description": "Worktree finalization target: 'current' (default), 'none', or a branch name."},
    "branch_name": {"type": "string", "description": "Override the persistent storage branch name."},
    "max_cost_usd":          {"type": "number",  "description": "Budget override: max cost in USD."},
    "max_tokens":            {"type": "integer", "description": "Budget override: max tokens."},
    "max_duration":          {"type": "string",  "description": "Budget override: max active duration (e.g. '1h')."},
    "max_iterations":        {"type": "integer", "description": "Budget override: max loop iterations."},
    "max_parallel_branches": {"type": "integer", "description": "Budget override: max parallel branches."}
  },
  "required": ["file_path"],
  "additionalProperties": false
}`),
			handler: handleLocalRun,
		},
		{
			Name:        "local_resume",
			Description: "Resume a paused / failed_resumable / cancelled local run in a detached subprocess. The workflow file defaults to the path captured at launch; pass answers to unblock a run paused on human questions.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_id":    {"type": "string"},
    "file_path": {"type": "string", "description": "Workflow file override (defaults to the run's recorded file_path)."},
    "answers":   {"type": "object", "description": "Answers for a run paused on human input, keyed by question/field id.", "additionalProperties": {"type": "string"}},
    "force":     {"type": "boolean", "description": "Allow resume when the .bot source changed since launch."}
  },
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleLocalResume,
		},
		{
			Name:        "local_run_cancel",
			Description: "Cancel a detached local run by SIGTERM-ing its runner process group (recorded in the run's .pid file). Runs owned by another surface (a studio's in-process run) have no .pid here and must be cancelled from that surface.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"run_id": {"type": "string"}},
  "required": ["run_id"],
  "additionalProperties": false
}`),
			handler: handleLocalRunCancel,
		},
		{
			Name:        "local_answer",
			Description: "Answer one pending async question on a local run (non-blocking ask_user_async — the run keeps working and picks the answer up at its next turn).",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_id":         {"type": "string"},
    "interaction_id": {"type": "string"},
    "answer":         {"type": "string"}
  },
  "required": ["run_id", "interaction_id", "answer"],
  "additionalProperties": false
}`),
			handler: handleLocalAnswer,
		},
	}
}

// resolvePath anchors a relative path on the server's working directory
// so tool behavior doesn't depend on the MCP client's spawn cwd.
func (s *Server) resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.WorkDir, p)
}

func handleLocalValidate(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		FilePath string `json:"file_path"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.FilePath == "" {
		return "", false, fmt.Errorf("file_path is required")
	}
	out, err := captureJSON(func(p *cli.Printer) error {
		return cli.RunValidate(s.resolvePath(args.FilePath), p)
	})
	// RunValidate returns "validation failed" AFTER printing the result
	// JSON — an invalid workflow is a normal answer for this tool, so
	// surface the diagnostics-bearing JSON, not an error. A pre-print
	// failure (missing file, unreadable bundle) has no output and stays
	// an error.
	if err != nil && out == "" {
		return "", false, err
	}
	return out, false, nil
}

func handleLocalBotsList(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Paths []string `json:"paths"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	paths := args.Paths
	if len(paths) == 0 {
		paths = []string{"bots", "examples"}
	}
	existing := make([]string, 0, len(paths))
	for _, p := range paths {
		rp := s.resolvePath(p)
		if _, err := os.Stat(rp); err == nil {
			existing = append(existing, rp)
		}
	}
	if len(existing) == 0 {
		return fmt.Sprintf("[]\n(no bot paths found under %s: looked for %s)", s.WorkDir, strings.Join(paths, ", ")), false, nil
	}
	var buf strings.Builder
	if err := cli.BotsList(cli.BotsListOptions{Paths: existing, Format: "json"}, &buf); err != nil {
		return "", false, err
	}
	return strings.TrimSpace(buf.String()), false, nil
}

// runSummary is the compact per-run projection local_runs_list returns.
type runSummary struct {
	ID           string     `json:"id"`
	Name         string     `json:"name,omitempty"`
	WorkflowName string     `json:"workflow_name"`
	Bot          string     `json:"bot,omitempty"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Error        string     `json:"error,omitempty"`
}

func handleLocalRunsList(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		Status   string `json:"status"`
		Workflow string `json:"workflow"`
		Limit    int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	st, err := s.store()
	if err != nil {
		return "", false, err
	}
	ids, err := st.ListRuns(ctx)
	if err != nil {
		return "", false, err
	}
	summaries := make([]runSummary, 0, len(ids))
	var loadErrs int
	for _, id := range ids {
		r, err := st.LoadRun(ctx, id)
		if err != nil {
			loadErrs++
			continue
		}
		if args.Status != "" && string(r.Status) != args.Status {
			continue
		}
		if args.Workflow != "" && r.WorkflowName != args.Workflow {
			continue
		}
		bot := r.BundleDisplayName
		if bot == "" {
			bot = r.BundleName
		}
		summaries = append(summaries, runSummary{
			ID:           r.ID,
			Name:         r.Name,
			WorkflowName: r.WorkflowName,
			Bot:          bot,
			Status:       string(r.Status),
			CreatedAt:    r.CreatedAt,
			FinishedAt:   r.FinishedAt,
			Error:        r.Error,
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].CreatedAt.After(summaries[j].CreatedAt) })
	total := len(summaries)
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	out, err := marshalText(map[string]any{
		"runs":       summaries,
		"total":      total,
		"unreadable": loadErrs,
		"store_dir":  s.StoreDir,
		"truncated":  total > limit,
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleLocalRunGet(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	st, err := s.store()
	if err != nil {
		return "", false, err
	}
	r, err := st.LoadRun(ctx, args.RunID)
	if err != nil {
		return "", false, err
	}
	view := map[string]any{
		"id":            r.ID,
		"name":          r.Name,
		"workflow_name": r.WorkflowName,
		"file_path":     r.FilePath,
		"status":        string(r.Status),
		"created_at":    r.CreatedAt,
		"updated_at":    r.UpdatedAt,
		"store_dir":     s.StoreDir,
	}
	if r.FinishedAt != nil {
		view["finished_at"] = *r.FinishedAt
	}
	if r.Error != "" {
		view["error"] = r.Error
	}
	if r.Budget != nil {
		view["budget"] = r.Budget
	}
	if r.BundleName != "" {
		view["bundle_name"] = r.BundleName
	}
	if r.BundleDisplayName != "" {
		view["bot"] = r.BundleDisplayName
	}
	if r.WorkDir != "" {
		view["work_dir"] = r.WorkDir
		view["worktree"] = r.Worktree
	}
	if r.FinalCommit != "" {
		view["final_commit"] = r.FinalCommit
	}
	if r.FinalBranch != "" {
		view["final_branch"] = r.FinalBranch
	}
	if r.MergedInto != "" {
		view["merged_into"] = r.MergedInto
	}
	if r.MergeStatus != "" {
		view["merge_status"] = string(r.MergeStatus)
	}
	view["resumable"] = r.Status == store.RunStatusFailedResumable || r.Status == store.RunStatusCancelled || r.Status.IsPaused()

	// Detached-runner liveness: a "running" doc whose recorded runner
	// process is gone means the run died without reaching a terminal
	// status (SIGKILL, reboot). Reported, never silently repaired —
	// resume it or cancel it explicitly.
	if r.Status == store.RunStatusRunning {
		if pid, alive, err := detachedRunnerState(st, r.ID); err == nil && pid > 0 {
			view["runner_pid"] = pid
			view["runner_alive"] = alive
			if !alive {
				view["warning"] = "run doc says running but the recorded runner process is gone — the run likely died; resume it (local_resume) or inspect events"
			}
		}
	}
	out, err := marshalText(view)
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleLocalRunEvents(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
		Since int64  `json:"since"`
		Limit int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	st, err := s.store()
	if err != nil {
		return "", false, err
	}
	events, err := st.LoadEventsRange(ctx, args.RunID, args.Since, 0, limit)
	if err != nil {
		return "", false, err
	}
	out, err := marshalText(map[string]any{"events": events, "count": len(events)})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

// localRunLogCap bounds the bytes a single local_run_log call returns so
// a multi-MB run.log can't blow up the client's context window.
const localRunLogCap = 128 * 1024

func handleLocalRunLog(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
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
	tail := args.Tail
	if tail <= 0 {
		tail = 200
	}
	st, err := s.store()
	if err != nil {
		return "", false, err
	}
	logs := store.AsRunLogStore(st)
	if logs == nil {
		return "", false, fmt.Errorf("run store %s does not persist run logs", s.StoreDir)
	}
	size, err := logs.RunLogSize(ctx, args.RunID)
	if err != nil {
		return "", false, err
	}
	if size == 0 {
		return "(run produced no log yet)", false, nil
	}
	// Read at most the trailing cap window — tailing lines never needs
	// the full file.
	from := int64(0)
	if size > localRunLogCap {
		from = size - localRunLogCap
	}
	data, err := logs.ReadRunLogRange(ctx, args.RunID, from, 0)
	if err != nil {
		return "", false, err
	}
	lines := strings.Split(string(data), "\n")
	truncatedByWindow := from > 0
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
		truncatedByWindow = true
	}
	text := strings.Join(lines, "\n")
	if truncatedByWindow {
		text = fmt.Sprintf("(truncated: showing the last %d lines of %d logged bytes)\n%s", len(lines), size, text)
	}
	return text, false, nil
}

func handleLocalRunReport(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	// RunReport writes markdown to a file (stdout only carries the
	// confirmation line), so route it through a temp file to return the
	// report body itself.
	tmp, err := os.CreateTemp("", "iterion-mcp-report-*.md")
	if err != nil {
		return "", false, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := captureHuman(func(p *cli.Printer) error {
		return cli.RunReport(cli.ReportOptions{RunID: args.RunID, StoreDir: s.StoreDir, Output: tmpPath}, p)
	}); err != nil {
		return "", false, err
	}
	md, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", false, err
	}
	return string(md), false, nil
}

func handleLocalQuestions(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	out, err := captureJSON(func(p *cli.Printer) error {
		return cli.RunQuestions(cli.QuestionsOptions{RunID: args.RunID, StoreDir: s.StoreDir}, p)
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleLocalAnswer(_ context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID         string `json:"run_id"`
		InteractionID string `json:"interaction_id"`
		Answer        string `json:"answer"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" || args.InteractionID == "" || args.Answer == "" {
		return "", false, fmt.Errorf("run_id, interaction_id and answer are required")
	}
	out, err := captureJSON(func(p *cli.Printer) error {
		return cli.RunAnswer(cli.AnswerOptions{
			RunID:         args.RunID,
			InteractionID: args.InteractionID,
			Answer:        args.Answer,
			StoreDir:      s.StoreDir,
		}, p)
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleLocalRun(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		FilePath            string            `json:"file_path"`
		Vars                map[string]string `json:"vars"`
		Timeout             string            `json:"timeout"`
		MergeInto           string            `json:"merge_into"`
		BranchName          string            `json:"branch_name"`
		MaxCostUSD          float64           `json:"max_cost_usd"`
		MaxTokens           int               `json:"max_tokens"`
		MaxDuration         string            `json:"max_duration"`
		MaxIterations       int               `json:"max_iterations"`
		MaxParallelBranches int               `json:"max_parallel_branches"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.FilePath == "" {
		return "", false, fmt.Errorf("file_path is required")
	}
	filePath := s.resolvePath(args.FilePath)

	// Synchronous pre-flight: reject an invalid workflow here with the
	// full diagnostics instead of letting the detached runner die out of
	// sight (same rationale as the studio's launchDetached pre-compile).
	validateOut, verr := captureJSON(func(p *cli.Printer) error {
		return cli.RunValidate(filePath, p)
	})
	if verr != nil {
		if validateOut != "" {
			return "workflow validation failed:\n" + validateOut, true, nil
		}
		return "", false, verr
	}
	var vres cli.ValidateResult
	if err := json.Unmarshal([]byte(validateOut), &vres); err != nil {
		return "", false, fmt.Errorf("parse validation result: %w", err)
	}

	st, err := s.store()
	if err != nil {
		return "", false, err
	}
	runID, err := store.GenerateRunID()
	if err != nil {
		return "", false, err
	}

	// Pre-create the run doc so a local_run_get issued right after this
	// call never 404s while the runner subprocess is still forking; the
	// runner's engine claims this doc instead of re-creating it.
	inputs := make(map[string]any, len(args.Vars))
	for k, v := range args.Vars {
		inputs[k] = v
	}
	if _, err := st.CreateRun(ctx, runID, vres.WorkflowName, inputs); err != nil {
		return "", false, fmt.Errorf("create run doc: %w", err)
	}

	spec := runnerSpec{
		Command:  runnerCommandRun,
		RunID:    runID,
		FilePath: filePath,
		StoreDir: s.StoreDir,
		Vars:     args.Vars,
		Timeout:  args.Timeout,

		MergeInto:           args.MergeInto,
		BranchName:          args.BranchName,
		MaxCostUSD:          args.MaxCostUSD,
		MaxTokens:           args.MaxTokens,
		MaxDuration:         args.MaxDuration,
		MaxIterations:       args.MaxIterations,
		MaxParallelBranches: args.MaxParallelBranches,
	}
	pid, err := spawnDetachedRunner(st, spec)
	if err != nil {
		// Without a runner the pre-created doc would sit as a phantom
		// "running" run forever — mark it failed, visibly.
		if uerr := st.UpdateRunStatus(ctx, runID, store.RunStatusFailed, "runner failed to start: "+err.Error()); uerr != nil {
			return "", false, fmt.Errorf("start runner: %w (and marking the run failed also failed: %v)", err, uerr)
		}
		return "", false, fmt.Errorf("start runner: %w", err)
	}

	out, err := marshalText(map[string]any{
		"run_id":     runID,
		"workflow":   vres.WorkflowName,
		"status":     "running",
		"runner_pid": pid,
		"store_dir":  s.StoreDir,
		"detached":   true,
		"follow":     "poll local_run_get / local_run_events (since=<last seq>); the run survives this MCP session",
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleLocalResume(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID    string            `json:"run_id"`
		FilePath string            `json:"file_path"`
		Answers  map[string]string `json:"answers"`
		Force    bool              `json:"force"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	st, err := s.store()
	if err != nil {
		return "", false, err
	}
	r, err := st.LoadRun(ctx, args.RunID)
	if err != nil {
		return "", false, err
	}
	if !r.Status.IsPaused() && r.Status != store.RunStatusFailedResumable && r.Status != store.RunStatusCancelled {
		return "", false, fmt.Errorf("run %s has status %s — only paused, failed_resumable or cancelled runs can be resumed", r.ID, r.Status)
	}
	filePath := s.resolvePath(args.FilePath)
	if filePath == "" {
		filePath = r.FilePath
	}
	if filePath == "" {
		return "", false, fmt.Errorf("run %s recorded no workflow file path — pass file_path explicitly", r.ID)
	}
	if pid, alive, err := detachedRunnerState(st, r.ID); err == nil && pid > 0 && alive {
		return "", false, fmt.Errorf("run %s already has a live runner process (pid %d)", r.ID, pid)
	}

	spec := runnerSpec{
		Command:  runnerCommandResume,
		RunID:    args.RunID,
		FilePath: filePath,
		StoreDir: s.StoreDir,
		Answers:  args.Answers,
		Force:    args.Force,
	}
	pid, err := spawnDetachedRunner(st, spec)
	if err != nil {
		return "", false, fmt.Errorf("start runner: %w", err)
	}
	out, err := marshalText(map[string]any{
		"run_id":     args.RunID,
		"status":     "resuming",
		"runner_pid": pid,
		"detached":   true,
		"follow":     "poll local_run_get / local_run_events",
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

func handleLocalRunCancel(ctx context.Context, s *Server, raw json.RawMessage) (string, bool, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", false, err
	}
	if args.RunID == "" {
		return "", false, fmt.Errorf("run_id is required")
	}
	st, err := s.store()
	if err != nil {
		return "", false, err
	}
	r, err := st.LoadRun(ctx, args.RunID)
	if err != nil {
		return "", false, err
	}
	if r.Status.IsTerminal() {
		return "", false, fmt.Errorf("run %s is already %s", r.ID, r.Status)
	}
	pid, alive, err := detachedRunnerState(st, r.ID)
	if err != nil || pid <= 0 {
		return "", false, fmt.Errorf("run %s has no .pid file in this store — it is not a detached run owned here; cancel it from its owning surface (studio, or the terminal running it)", r.ID)
	}
	if !alive {
		return "", false, fmt.Errorf("run %s's recorded runner process (pid %d) is already gone; the run doc still says %s — resume it or inspect events", r.ID, pid, r.Status)
	}
	if err := terminateProcessGroup(pid); err != nil {
		return "", false, fmt.Errorf("signal runner process group %d: %w", pid, err)
	}
	out, err := marshalText(map[string]any{
		"run_id":     r.ID,
		"runner_pid": pid,
		"signalled":  "SIGTERM (process group)",
		"follow":     "poll local_run_get until status becomes cancelled",
	})
	if err != nil {
		return "", false, err
	}
	return out, false, nil
}

// detachedRunnerState reads a run's .pid file and probes liveness.
// pid == 0 with a nil error means "no detached runner recorded" —
// either the store keeps no PID files or this run has none
// (ReadPIDFile reports a missing file as (0, nil)).
func detachedRunnerState(st *store.FilesystemRunStore, runID string) (pid int, alive bool, err error) {
	pidS := store.AsPIDStore(st)
	if pidS == nil {
		return 0, false, nil
	}
	pid, err = pidS.ReadPIDFile(runID)
	if err != nil {
		return 0, false, err
	}
	return pid, pidAlive(pid) == nil, nil
}
