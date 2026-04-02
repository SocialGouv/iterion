package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/store"
)

// ReportOptions holds the configuration for the report command.
type ReportOptions struct {
	RunID    string
	StoreDir string
	Output   string // output file path (empty = stdout)
}

// RunReport generates a detailed chronological report for a run.
func RunReport(opts ReportOptions, p *Printer) error {
	storeDir := opts.StoreDir
	if storeDir == "" {
		storeDir = ".iterion"
	}

	s, err := store.New(storeDir)
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	if opts.RunID == "" {
		return fmt.Errorf("--run-id is required")
	}

	r, err := s.LoadRun(opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	events, err := s.LoadEvents(opts.RunID)
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
	RunID        string         `json:"run_id"`
	Workflow     string         `json:"workflow"`
	Status       string         `json:"status"`
	Duration     string         `json:"duration"`
	CreatedAt    time.Time      `json:"created_at"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
	Error        string         `json:"error,omitempty"`
	Metrics      reportMetrics  `json:"metrics"`
	Steps        []reportStep   `json:"steps"`
	Artifacts    []reportArtifact `json:"artifacts"`
}

type reportMetrics struct {
	TotalTokens  int     `json:"total_tokens"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	ModelCalls   int     `json:"model_calls"`
	NodeCount    int     `json:"node_count"`
	LoopEdges    int     `json:"loop_edges"`
}

type reportStep struct {
	Seq       int64     `json:"seq"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	NodeID    string    `json:"node_id,omitempty"`
	BranchID  string    `json:"branch_id,omitempty"`
	Summary   string    `json:"summary"`
	Detail    string    `json:"detail,omitempty"`
	Tokens    int       `json:"tokens,omitempty"`
	CostUSD   float64   `json:"cost_usd,omitempty"`
}

type reportArtifact struct {
	NodeID  string `json:"node_id"`
	Version int    `json:"version"`
	Summary string `json:"summary,omitempty"`
}

// ---------------------------------------------------------------------------
// Build report from events
// ---------------------------------------------------------------------------

func buildReport(r *store.Run, events []*store.Event, s *store.RunStore) *report {
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

	nodeSet := make(map[string]bool)
	stepNum := 0

	for _, evt := range events {
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

		switch evt.Type {
		case store.EventRunStarted:
			step.Summary = "Run started"

		case store.EventNodeStarted:
			stepNum++
			kind := ""
			if evt.Data != nil {
				if k, ok := evt.Data["kind"].(string); ok {
					kind = k
				}
			}
			step.Summary = fmt.Sprintf("Step %d: %s (%s)", stepNum, evt.NodeID, kind)
			nodeSet[evt.NodeID] = true

			if evt.Data != nil {
				if idx, ok := evt.Data["round_robin_index"]; ok {
					step.Detail = fmt.Sprintf("Round-robin index: %v → %v", idx, evt.Data["selected_target"])
				}
			}

		case store.EventLLMPrompt:
			if evt.Data == nil {
				continue
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

		case store.EventLLMStepFinished:
			if evt.Data == nil {
				continue
			}
			respLen := 0
			if resp, ok := evt.Data["response_text"].(string); ok {
				respLen = len(resp)
			}
			tokens := extractTokens(evt.Data)
			step.Summary = fmt.Sprintf("LLM response [%s] (%d chars)", evt.NodeID, respLen)
			step.Tokens = tokens

		case store.EventNodeFinished:
			if evt.Data == nil {
				continue
			}
			tokens := extractTokens(evt.Data)
			cost := extractCost(evt.Data)
			rpt.Metrics.TotalTokens += tokens
			rpt.Metrics.TotalCostUSD += cost
			rpt.Metrics.NodeCount++

			summary := ""
			if output, ok := evt.Data["output"]; ok {
				if outMap, ok := output.(map[string]interface{}); ok {
					if s, ok := outMap["summary"].(string); ok {
						summary = truncate(s, 200)
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

			delegate := ""
			if d, ok := evt.Data["_delegate"].(string); ok {
				delegate = fmt.Sprintf(" [%s]", d)
			}
			step.Summary = fmt.Sprintf("Finished: %s%s", evt.NodeID, delegate)
			if summary != "" {
				step.Detail = summary
			}
			step.Tokens = tokens
			step.CostUSD = cost

		case store.EventEdgeSelected:
			if evt.Data == nil {
				continue
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
				iter, _ := evt.Data["iteration"]
				info += fmt.Sprintf(" [loop: %s, iter: %v]", loop, iter)
				rpt.Metrics.LoopEdges++
			}
			step.Summary = "Edge: " + info

		case store.EventBranchStarted:
			step.Summary = fmt.Sprintf("Branch started: %s → %s", evt.BranchID, evt.NodeID)

		case store.EventJoinReady:
			step.Summary = fmt.Sprintf("Join ready: %s", evt.NodeID)

		case store.EventArtifactWritten:
			if evt.Data != nil {
				step.Summary = fmt.Sprintf("Artifact: %s (publish: %v, version: %v)", evt.NodeID, evt.Data["publish"], evt.Data["version"])
			}

		case store.EventBudgetWarning:
			if evt.Data != nil {
				step.Summary = fmt.Sprintf("Budget warning: %v (used: %v / limit: %v)", evt.Data["dimension"], evt.Data["used"], evt.Data["limit"])
			}

		case store.EventRunFinished:
			step.Summary = "Run finished"

		case store.EventRunFailed:
			if evt.Data != nil {
				step.Summary = fmt.Sprintf("Run failed: %v: %v", evt.Data["code"], evt.Data["error"])
			} else {
				step.Summary = "Run failed"
			}

		case store.EventLLMRequest:
			rpt.Metrics.ModelCalls++
			continue // skip adding to steps — too noisy

		default:
			step.Summary = fmt.Sprintf("%s [%s]", evt.Type, evt.NodeID)
		}

		rpt.Steps = append(rpt.Steps, step)
	}

	// Collect artifacts.
	artifactsDir := filepath.Join(s.Root(), "runs", r.ID, "artifacts")
	if entries, err := os.ReadDir(artifactsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			nodeID := entry.Name()
			art, err := s.LoadLatestArtifact(r.ID, nodeID)
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

	return rpt
}

// ---------------------------------------------------------------------------
// Render markdown
// ---------------------------------------------------------------------------

func renderMarkdown(rpt *report) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# Run Report: %s\n\n", rpt.RunID))

	// Summary table.
	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("| Field | Value |\n"))
	sb.WriteString(fmt.Sprintf("|-------|-------|\n"))
	sb.WriteString(fmt.Sprintf("| Workflow | %s |\n", rpt.Workflow))
	sb.WriteString(fmt.Sprintf("| Status | %s |\n", rpt.Status))
	sb.WriteString(fmt.Sprintf("| Duration | %s |\n", rpt.Duration))
	sb.WriteString(fmt.Sprintf("| Total Tokens | %d |\n", rpt.Metrics.TotalTokens))
	if rpt.Metrics.TotalCostUSD > 0 {
		sb.WriteString(fmt.Sprintf("| Total Cost | $%.4f |\n", rpt.Metrics.TotalCostUSD))
	}
	sb.WriteString(fmt.Sprintf("| Model Calls | %d |\n", rpt.Metrics.ModelCalls))
	sb.WriteString(fmt.Sprintf("| Node Executions | %d |\n", rpt.Metrics.NodeCount))
	sb.WriteString(fmt.Sprintf("| Loop Edges | %d |\n", rpt.Metrics.LoopEdges))
	if rpt.Error != "" {
		sb.WriteString(fmt.Sprintf("| Error | %s |\n", rpt.Error))
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
			sb.WriteString(fmt.Sprintf("| %s | v%d | %s |\n", art.NodeID, art.Version, summary))
		}
		sb.WriteString("\n")
	}

	// Chronological timeline.
	sb.WriteString("## Timeline\n\n")

	for _, step := range rpt.Steps {
		ts := step.Time.Format("15:04:05")

		switch {
		case step.Type == string(store.EventRunStarted):
			sb.WriteString(fmt.Sprintf("### %s — Run Started\n\n", ts))

		case step.Type == string(store.EventNodeStarted):
			branch := ""
			if step.BranchID != "" {
				branch = fmt.Sprintf(" `[%s]`", step.BranchID)
			}
			sb.WriteString(fmt.Sprintf("### %s — %s%s\n\n", ts, step.Summary, branch))
			if step.Detail != "" {
				sb.WriteString(fmt.Sprintf("> %s\n\n", step.Detail))
			}

		case step.Type == string(store.EventNodeFinished):
			tokens := ""
			if step.Tokens > 0 {
				tokens = fmt.Sprintf(" (%d tokens)", step.Tokens)
			}
			sb.WriteString(fmt.Sprintf("- **%s** %s%s\n", ts, step.Summary, tokens))
			if step.Detail != "" {
				sb.WriteString(fmt.Sprintf("  > %s\n", step.Detail))
			}
			sb.WriteString("\n")

		case step.Type == string(store.EventEdgeSelected):
			sb.WriteString(fmt.Sprintf("- %s → %s\n", ts, step.Summary))

		case step.Type == string(store.EventBranchStarted):
			sb.WriteString(fmt.Sprintf("- %s 🔀 %s\n", ts, step.Summary))

		case step.Type == string(store.EventJoinReady):
			sb.WriteString(fmt.Sprintf("- %s 🔗 %s\n", ts, step.Summary))

		case step.Type == string(store.EventArtifactWritten):
			sb.WriteString(fmt.Sprintf("- %s 📦 %s\n", ts, step.Summary))

		case step.Type == string(store.EventRunFinished):
			sb.WriteString(fmt.Sprintf("\n### %s — Run Finished\n", ts))

		case step.Type == string(store.EventRunFailed):
			sb.WriteString(fmt.Sprintf("\n### %s — Run Failed\n\n> %s\n", ts, step.Summary))

		case step.Type == string(store.EventBudgetWarning):
			sb.WriteString(fmt.Sprintf("- %s ⚠️ %s\n", ts, step.Summary))

		// Skip LLM prompt/response details in timeline (too verbose).
		case step.Type == string(store.EventLLMPrompt),
			step.Type == string(store.EventLLMStepFinished):
			// omit from markdown — the node_finished captures the essence

		default:
			sb.WriteString(fmt.Sprintf("- %s %s\n", ts, step.Summary))
		}
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractTokens(data map[string]interface{}) int {
	if v, ok := data["_tokens"]; ok {
		switch t := v.(type) {
		case float64:
			return int(t)
		case int:
			return t
		case json.Number:
			n, _ := t.Int64()
			return int(n)
		}
	}
	return 0
}

func extractCost(data map[string]interface{}) float64 {
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
	return s[:max] + "..."
}
