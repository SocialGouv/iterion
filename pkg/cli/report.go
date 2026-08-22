package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ReportOptions holds the configuration for the report command.
type ReportOptions struct {
	RunID    string
	StoreDir string
	Output   string // output file path (empty = stdout)
}

// RunReport generates a detailed chronological report for a run.
func RunReport(opts ReportOptions, p *Printer) error {
	cwd, _ := os.Getwd()
	storeDir := store.ResolveStoreDir(cwd, opts.StoreDir)

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	if opts.RunID == "" {
		return fmt.Errorf("--run-id is required")
	}

	// LoadRun / LoadEvents now sanitise the run ID, so any traversal
	// attempt (e.g. "../etc/cron.d") fails closed before touching the FS.
	r, err := s.LoadRun(context.Background(), opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	events, err := s.LoadEvents(context.Background(), opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load events: %w", err)
	}

	report := buildReport(r, events, s)

	if p.Format == OutputJSON {
		p.JSON(report)
		return nil
	}

	md := renderMarkdown(report)

	if opts.Output != "" {
		if err := os.WriteFile(opts.Output, []byte(md), 0o644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
		p.Line("Report written to %s", opts.Output)
		return nil
	}

	// Write to store by default.
	reportPath := filepath.Join(storeDir, "runs", opts.RunID, "report.md")
	if err := os.WriteFile(reportPath, []byte(md), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	p.Line("Report written to %s", reportPath)

	return nil
}

// ---------------------------------------------------------------------------
// Report data structures
// ---------------------------------------------------------------------------

type report struct {
	RunID    string `json:"run_id"`
	Workflow string `json:"workflow"`
	Status   string `json:"status"`
	Duration string `json:"duration"`
	// VerifyCommand is the build+test command the run's verify gate settled
	// on (e.g. "devbox run -- go build ./..."), lifted from the verify
	// authoring node's summary so it is greppable in the report header
	// instead of buried in node output. Empty when the run has no verify node.
	VerifyCommand string           `json:"verify_command,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	FinishedAt    *time.Time       `json:"finished_at,omitempty"`
	Error         string           `json:"error,omitempty"`
	Metrics       reportMetrics    `json:"metrics"`
	Steps         []reportStep     `json:"steps"`
	Artifacts     []reportArtifact `json:"artifacts"`
	// LocAdded / LocDeleted: three-dot numstat of the run's commits
	// against the fork point. Nil when the refs are unresolvable —
	// rendered as absent, never a guessed zero.
	LocAdded   *int `json:"loc_added,omitempty"`
	LocDeleted *int `json:"loc_deleted,omitempty"`
}

type reportMetrics struct {
	TotalTokens      int     `json:"total_tokens"`
	TotalInputTokens int     `json:"total_input_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	ThinkingTokens   int     `json:"thinking_tokens"`
	ThinkingMs       int     `json:"thinking_ms"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	ModelCalls       int     `json:"model_calls"`
	NodeCount        int     `json:"node_count"`
	LoopEdges        int     `json:"loop_edges"`
}

type reportStep struct {
	Seq      int64     `json:"seq"`
	Time     time.Time `json:"time"`
	Type     string    `json:"type"`
	NodeID   string    `json:"node_id,omitempty"`
	BranchID string    `json:"branch_id,omitempty"`
	Summary  string    `json:"summary"`
	Detail   string    `json:"detail,omitempty"`
	Tokens   int       `json:"tokens,omitempty"`
	CostUSD  float64   `json:"cost_usd,omitempty"`
}

type reportArtifact struct {
	NodeID  string `json:"node_id"`
	Version int    `json:"version"`
	Summary string `json:"summary,omitempty"`
}

// ---------------------------------------------------------------------------
// Build report from events
// ---------------------------------------------------------------------------

func buildReport(r *store.Run, events []*store.Event, s store.RunStore) *report {
	rpt := &report{
		RunID:     r.ID,
		Workflow:  r.WorkflowName,
		Status:    string(r.Status),
		CreatedAt: r.CreatedAt,
		Error:     r.Error,
	}

	if r.FinishedAt != nil {
		rpt.FinishedAt = r.FinishedAt
		rpt.Duration = FormatDuration(r.FinishedAt.Sub(r.CreatedAt))
	}

	// LOC changed by the run's commits (worktree runs with a recorded
	// FinalCommit only). Same three-dot semantics as the studio header.
	if r.FinalCommit != "" && r.RepoRoot != "" {
		target := r.MergedInto
		if target == "" || target == "none" {
			target = r.BaseCommit
		}
		if added, deleted, ok := gitlib.DiffLOC(r.RepoRoot, target, r.FinalCommit); ok {
			rpt.LocAdded, rpt.LocDeleted = &added, &deleted
		}
	}

	rb := &reportBuilder{rpt: rpt, nodeSet: make(map[string]bool)}
	for _, evt := range events {
		rb.consume(evt)
	}

	collectArtifacts(rpt, r, s)
	return rpt
}

// reportBuilder accumulates per-event state (stepNum, distinct nodeSet) and
// the running metrics + steps slice on rpt. consume() dispatches each event
// to its kind-specific summarizer; a summarizer returns false to skip
// appending the step (used by EventLLMRequest, which only bumps a counter,
// and by nil-data variants of LLMPrompt/LLMStepFinished/NodeFinished/
// EdgeSelected that have nothing to render).
type reportBuilder struct {
	rpt     *report
	nodeSet map[string]bool
	stepNum int
}

// consume processes a single event, dispatching to the kind-specific
// summarizer and appending the step (unless the summarizer skips it).
func (rb *reportBuilder) consume(evt *store.Event) {
	step := reportStep{
		Seq:  evt.Seq,
		Time: evt.Timestamp,
		Type: string(evt.Type),
	}
	if evt.NodeID != "" {
		step.NodeID = evt.NodeID
	}
	if evt.BranchID != "" {
		step.BranchID = evt.BranchID
	}
	if !rb.summarize(evt, &step) {
		return
	}
	rb.rpt.Steps = append(rb.rpt.Steps, step)
}

// summarize fills step.Summary (and possibly Detail/Tokens/CostUSD) for evt,
// updates rb.rpt.Metrics where applicable, and returns false when the step
// should NOT be appended (LLM-request counter-only events; nil-data events
// that carry no rendered content).
func (rb *reportBuilder) summarize(evt *store.Event, step *reportStep) bool {
	switch evt.Type {
	case store.EventRunStarted:
		return rb.sumRunStarted(step)
	case store.EventNodeStarted:
		return rb.sumNodeStarted(evt, step)
	case store.EventLLMPrompt:
		return rb.sumLLMPrompt(evt, step)
	case store.EventLLMStepFinished:
		return rb.sumLLMStepFinished(evt, step)
	case store.EventNodeFinished:
		return rb.sumNodeFinished(evt, step)
	case store.EventEdgeSelected:
		return rb.sumEdgeSelected(evt, step)
	case store.EventBranchStarted:
		return rb.sumBranchStarted(evt, step)
	case store.EventJoinReady:
		return rb.sumJoinReady(evt, step)
	case store.EventArtifactWritten:
		return rb.sumArtifactWritten(evt, step)
	case store.EventBudgetWarning:
		return rb.sumBudgetWarning(evt, step)
	case store.EventRunFinished:
		return rb.sumRunFinished(step)
	case store.EventRunFailed:
		return rb.sumRunFailed(evt, step)
	case store.EventLLMRequest:
		return rb.sumLLMRequest()
	default:
		step.Summary = fmt.Sprintf("%s [%s]", evt.Type, evt.NodeID)
		return true
	}
}

func (rb *reportBuilder) sumRunStarted(step *reportStep) bool {
	step.Summary = "Run started"
	return true
}

func (rb *reportBuilder) sumNodeStarted(evt *store.Event, step *reportStep) bool {
	rb.stepNum++
	kind := ""
	if evt.Data != nil {
		if k, ok := evt.Data["kind"].(string); ok {
			kind = k
		}
	}
	step.Summary = fmt.Sprintf("Step %d: %s (%s)", rb.stepNum, evt.NodeID, kind)
	rb.nodeSet[evt.NodeID] = true
	if evt.Data != nil {
		if idx, ok := evt.Data["round_robin_index"]; ok {
			step.Detail = fmt.Sprintf("Round-robin index: %v → %v", idx, evt.Data["selected_target"])
		}
	}
	return true
}

func (rb *reportBuilder) sumLLMPrompt(evt *store.Event, step *reportStep) bool {
	if evt.Data == nil {
		return false
	}
	sysLen := 0
	usrLen := 0
	if sys, ok := evt.Data["system_prompt"].(string); ok {
		sysLen = len(sys)
	}
	if usr, ok := evt.Data["user_message"].(string); ok {
		usrLen = len(usr)
	}
	step.Summary = fmt.Sprintf("LLM prompt [%s] (system: %d chars, user: %d chars)", evt.NodeID, sysLen, usrLen)
	return true
}

func (rb *reportBuilder) sumLLMStepFinished(evt *store.Event, step *reportStep) bool {
	if evt.Data == nil {
		return false
	}
	respLen := 0
	if resp, ok := evt.Data["response_text"].(string); ok {
		respLen = len(resp)
	}
	tokens := extractTokens(evt.Data)
	step.Summary = fmt.Sprintf("LLM response [%s] (%d chars)", evt.NodeID, respLen)
	step.Tokens = tokens
	rb.rpt.Metrics.TotalInputTokens += extractInt(evt.Data, "input_tokens")
	rb.rpt.Metrics.CacheReadTokens += extractInt(evt.Data, "cache_read_tokens")
	rb.rpt.Metrics.CacheWriteTokens += extractInt(evt.Data, "cache_write_tokens")
	return true
}

func (rb *reportBuilder) sumNodeFinished(evt *store.Event, step *reportStep) bool {
	if evt.Data == nil {
		return false
	}
	tokens := extractTokens(evt.Data)
	cost := extractCost(evt.Data)
	rb.rpt.Metrics.TotalTokens += tokens
	rb.rpt.Metrics.TotalCostUSD += cost
	rb.rpt.Metrics.NodeCount++

	summary := ""
	if output, ok := evt.Data["output"]; ok {
		if outMap, ok := output.(map[string]any); ok {
			// Thinking metrics are stamped onto the node output by
			// stampDelegateOutputMeta for both backends (claude_code
			// never emits llm_step_finished), so node_finished is the
			// single canonical source — same as _tokens/_cost_usd.
			rb.rpt.Metrics.ThinkingTokens += extractInt(outMap, "_thinking_tokens")
			rb.rpt.Metrics.ThinkingMs += extractInt(outMap, "_thinking_ms")
			if s, ok := outMap["summary"].(string); ok {
				summary = truncate(s, 200)
				// The verify authoring node's contract is {prepared, summary}
				// where summary is "the command you settled on" — surface its
				// first line as the header `verify:` line (no new detection,
				// just the node's existing output).
				if _, isVerifyPlan := outMap["prepared"]; isVerifyPlan {
					if cmd := firstLine([]byte(s)); cmd != "" {
						rb.rpt.VerifyCommand = cmd
					}
				}
			}
			// For judge nodes
			if approved, ok := outMap["approved"].(bool); ok {
				conf := ""
				if c, ok := outMap["confidence"].(string); ok {
					conf = c
				}
				summary = fmt.Sprintf("approved=%v confidence=%s", approved, conf)
			}
			if ready, ok := outMap["ready"].(bool); ok {
				conf := ""
				if c, ok := outMap["confidence"].(string); ok {
					conf = c
				}
				summary = fmt.Sprintf("ready=%v confidence=%s", ready, conf)
			}
		}
	}

	backendTag := ""
	if d, ok := evt.Data["_backend"].(string); ok {
		backendTag = fmt.Sprintf(" [%s]", d)
	}
	step.Summary = fmt.Sprintf("Finished: %s%s", evt.NodeID, backendTag)
	if summary != "" {
		step.Detail = summary
	}
	step.Tokens = tokens
	step.CostUSD = cost
	return true
}

func (rb *reportBuilder) sumEdgeSelected(evt *store.Event, step *reportStep) bool {
	if evt.Data == nil {
		return false
	}
	from, _ := evt.Data["from"].(string)
	to, _ := evt.Data["to"].(string)
	info := fmt.Sprintf("%s → %s", from, to)
	if cond, ok := evt.Data["condition"].(string); ok {
		negated, _ := evt.Data["negated"].(bool)
		if negated {
			info += fmt.Sprintf(" (when NOT %s)", cond)
		} else {
			info += fmt.Sprintf(" (when %s)", cond)
		}
	}
	if loop, ok := evt.Data["loop"].(string); ok {
		iter := evt.Data["iteration"]
		info += fmt.Sprintf(" [loop: %s, iter: %v]", loop, iter)
		rb.rpt.Metrics.LoopEdges++
	}
	step.Summary = "Edge: " + info
	return true
}

func (rb *reportBuilder) sumBranchStarted(evt *store.Event, step *reportStep) bool {
	step.Summary = fmt.Sprintf("Branch started: %s → %s", evt.BranchID, evt.NodeID)
	return true
}

func (rb *reportBuilder) sumJoinReady(evt *store.Event, step *reportStep) bool {
	step.Summary = fmt.Sprintf("Join ready: %s", evt.NodeID)
	return true
}

func (rb *reportBuilder) sumArtifactWritten(evt *store.Event, step *reportStep) bool {
	if evt.Data != nil {
		step.Summary = fmt.Sprintf("Artifact: %s (publish: %v, version: %v)", evt.NodeID, evt.Data["publish"], evt.Data["version"])
	}
	return true
}

func (rb *reportBuilder) sumBudgetWarning(evt *store.Event, step *reportStep) bool {
	if evt.Data != nil {
		// Dimensions without an axis publish a detail instead of used/limit;
		// printing "used: <nil> / limit: <nil>" would lose the only content
		// the event carries.
		if detail, ok := evt.Data["detail"].(string); ok && detail != "" {
			step.Summary = fmt.Sprintf("Budget warning: %v — %s", evt.Data["dimension"], detail)
			return true
		}
		step.Summary = fmt.Sprintf("Budget warning: %v (used: %v / limit: %v)", evt.Data["dimension"], evt.Data["used"], evt.Data["limit"])
	}
	return true
}

func (rb *reportBuilder) sumRunFinished(step *reportStep) bool {
	step.Summary = "Run finished"
	return true
}

func (rb *reportBuilder) sumRunFailed(evt *store.Event, step *reportStep) bool {
	if evt.Data != nil {
		step.Summary = fmt.Sprintf("Run failed: %v: %v", evt.Data["code"], evt.Data["error"])
	} else {
		step.Summary = "Run failed"
	}
	return true
}

// sumLLMRequest only bumps the model-calls counter; the event itself is too
// noisy to surface in the timeline (one per LLM turn) so the step is
// skipped via the false return.
func (rb *reportBuilder) sumLLMRequest() bool {
	rb.rpt.Metrics.ModelCalls++
	return false
}

// collectArtifacts walks the run's artifacts directory and appends the
// latest version of each node's artifact to rpt.Artifacts (sorted by
// node ID for stable output).
func collectArtifacts(rpt *report, r *store.Run, s store.RunStore) {
	artifactsDir := filepath.Join(s.Root(), "runs", r.ID, "artifacts")
	entries, err := os.ReadDir(artifactsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		nodeID := entry.Name()
		art, err := s.LoadLatestArtifact(context.Background(), r.ID, nodeID)
		if err != nil {
			continue
		}
		summary := ""
		if s, ok := art.Data["summary"].(string); ok {
			summary = truncate(s, 150)
		}
		rpt.Artifacts = append(rpt.Artifacts, reportArtifact{
			NodeID:  nodeID,
			Version: art.Version,
			Summary: summary,
		})
	}
	sort.Slice(rpt.Artifacts, func(i, j int) bool {
		return rpt.Artifacts[i].NodeID < rpt.Artifacts[j].NodeID
	})
}

// ---------------------------------------------------------------------------
// Render markdown
// ---------------------------------------------------------------------------

func renderMarkdown(rpt *report) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "# Run Report: %s\n\n", rpt.RunID)

	// Resolved verify command, hoisted to the header so it is greppable at a
	// glance instead of buried in the verify node's output.
	if rpt.VerifyCommand != "" {
		fmt.Fprintf(&sb, "verify: %s\n\n", rpt.VerifyCommand)
	}

	// Summary table.
	sb.WriteString("## Summary\n\n")
	sb.WriteString("| Field | Value |\n")
	sb.WriteString("|-------|-------|\n")
	fmt.Fprintf(&sb, "| Workflow | %s |\n", rpt.Workflow)
	fmt.Fprintf(&sb, "| Status | %s |\n", rpt.Status)
	fmt.Fprintf(&sb, "| Duration | %s |\n", rpt.Duration)
	fmt.Fprintf(&sb, "| Total Tokens | %d |\n", rpt.Metrics.TotalTokens)
	if rpt.Metrics.TotalCostUSD > 0 {
		fmt.Fprintf(&sb, "| Total Cost | $%.4f |\n", rpt.Metrics.TotalCostUSD)
	}
	if rpt.LocAdded != nil && rpt.LocDeleted != nil {
		fmt.Fprintf(&sb, "| Lines Changed | +%d / −%d |\n", *rpt.LocAdded, *rpt.LocDeleted)
	}
	fmt.Fprintf(&sb, "| Model Calls | %d |\n", rpt.Metrics.ModelCalls)
	fmt.Fprintf(&sb, "| Node Executions | %d |\n", rpt.Metrics.NodeCount)
	fmt.Fprintf(&sb, "| Loop Edges | %d |\n", rpt.Metrics.LoopEdges)
	if rpt.Metrics.ThinkingTokens > 0 || rpt.Metrics.ThinkingMs > 0 {
		// Thinking tokens are an approximation (the provider bills thinking
		// inside output tokens with no breakdown — re-encoded here).
		fmt.Fprintf(&sb, "| Thinking Tokens | ~%d |\n", rpt.Metrics.ThinkingTokens)
		fmt.Fprintf(&sb, "| Thinking Time | %s |\n", FormatDuration(time.Duration(rpt.Metrics.ThinkingMs)*time.Millisecond))
	}
	if rpt.Metrics.CacheReadTokens > 0 || rpt.Metrics.CacheWriteTokens > 0 {
		fmt.Fprintf(&sb, "| Cache Read Tokens | %d |\n", rpt.Metrics.CacheReadTokens)
		fmt.Fprintf(&sb, "| Cache Write Tokens | %d |\n", rpt.Metrics.CacheWriteTokens)
		denom := rpt.Metrics.TotalInputTokens + rpt.Metrics.CacheReadTokens
		if denom > 0 {
			ratio := float64(rpt.Metrics.CacheReadTokens) * 100 / float64(denom)
			fmt.Fprintf(&sb, "| Cache Hit Ratio | %.1f%% |\n", ratio)
		}
	}
	if rpt.Error != "" {
		fmt.Fprintf(&sb, "| Error | %s |\n", rpt.Error)
	}
	sb.WriteString("\n")

	// Artifacts.
	if len(rpt.Artifacts) > 0 {
		sb.WriteString("## Artifacts\n\n")
		sb.WriteString("| Node | Version | Summary |\n")
		sb.WriteString("|------|---------|--------|\n")
		for _, art := range rpt.Artifacts {
			summary := art.Summary
			if summary == "" {
				summary = "—"
			}
			fmt.Fprintf(&sb, "| %s | v%d | %s |\n", art.NodeID, art.Version, summary)
		}
		sb.WriteString("\n")
	}

	// Chronological timeline.
	sb.WriteString("## Timeline\n\n")

	for _, step := range rpt.Steps {
		ts := step.Time.Format("15:04:05")

		switch {
		case step.Type == string(store.EventRunStarted):
			fmt.Fprintf(&sb, "### %s — Run Started\n\n", ts)

		case step.Type == string(store.EventNodeStarted):
			branch := ""
			if step.BranchID != "" {
				branch = fmt.Sprintf(" `[%s]`", step.BranchID)
			}
			fmt.Fprintf(&sb, "### %s — %s%s\n\n", ts, step.Summary, branch)
			if step.Detail != "" {
				fmt.Fprintf(&sb, "> %s\n\n", step.Detail)
			}

		case step.Type == string(store.EventNodeFinished):
			tokens := ""
			if step.Tokens > 0 {
				tokens = fmt.Sprintf(" (%d tokens)", step.Tokens)
			}
			fmt.Fprintf(&sb, "- **%s** %s%s\n", ts, step.Summary, tokens)
			if step.Detail != "" {
				fmt.Fprintf(&sb, "  > %s\n", step.Detail)
			}
			sb.WriteString("\n")

		case step.Type == string(store.EventEdgeSelected):
			fmt.Fprintf(&sb, "- %s → %s\n", ts, step.Summary)

		case step.Type == string(store.EventBranchStarted):
			fmt.Fprintf(&sb, "- %s 🔀 %s\n", ts, step.Summary)

		case step.Type == string(store.EventJoinReady):
			fmt.Fprintf(&sb, "- %s 🔗 %s\n", ts, step.Summary)

		case step.Type == string(store.EventArtifactWritten):
			fmt.Fprintf(&sb, "- %s 📦 %s\n", ts, step.Summary)

		case step.Type == string(store.EventRunFinished):
			fmt.Fprintf(&sb, "\n### %s — Run Finished\n", ts)

		case step.Type == string(store.EventRunFailed):
			fmt.Fprintf(&sb, "\n### %s — Run Failed\n\n> %s\n", ts, step.Summary)

		case step.Type == string(store.EventBudgetWarning):
			fmt.Fprintf(&sb, "- %s ⚠️ %s\n", ts, step.Summary)

		// Skip LLM prompt/response details in timeline (too verbose).
		case step.Type == string(store.EventLLMPrompt),
			step.Type == string(store.EventLLMStepFinished):
			// omit from markdown — the node_finished captures the essence

		default:
			fmt.Fprintf(&sb, "- %s %s\n", ts, step.Summary)
		}
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractTokens(data map[string]any) int {
	return extractInt(data, "_tokens")
}

func extractInt(data map[string]any, key string) int {
	v, ok := data[key]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}

func extractCost(data map[string]any) float64 {
	if v, ok := data["_cost_usd"]; ok {
		switch t := v.(type) {
		case float64:
			return t
		case json.Number:
			f, _ := t.Float64()
			return f
		}
	}
	return 0
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary: the for-range index always lands at the
	// start of a rune, so slicing there never splits a multi-byte UTF-8
	// sequence (which s[:max] would).
	for i := range s {
		if i >= max {
			return s[:i] + "..."
		}
	}
	return s
}
