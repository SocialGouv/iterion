// Package runops owns the capability-gated, read-only operations used by
// assistant and MCP transports to inspect runs. It depends only on
// store.RunStore, so the same calls operate on the local filesystem store and
// the tenant-scoped Mongo store without exposing either one's location.
package runops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

const CapRunsRead = "runs.read"

var ErrCapabilityDenied = errors.New("runops: capability denied")

type Capabilities map[string]bool

func NewCapabilities(names ...string) Capabilities {
	out := Capabilities{}
	for _, name := range names {
		for _, item := range strings.Split(name, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out[item] = true
			}
		}
	}
	return out
}

type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

var tools = []Tool{
	{
		Name:        "run_events",
		Description: "Read a bounded diagnostic projection of a run's event stream. Inputs, prompts and arbitrary tool/node outputs are omitted. Use since to continue from next_seq.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"},"since":{"type":"integer","description":"Only events with seq >= since."},"limit":{"type":"integer","description":"Maximum events (default 100, max 250)."}},"required":["run_id"],"additionalProperties":false}`),
	},
	{
		Name:        "run_get",
		Description: "Fetch a bounded run diagnosis: status, workflow, resumability, failing node, error code, error, and the run's lineage (parent_run_id, ancestors, root_run_id). Check `orphaned` before proposing a resume: it means no ancestor is still running, so resuming THIS run completes work nobody is waiting for — resume root_run_id instead. Does not expose inputs or filesystem paths.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string"}},"required":["run_id"],"additionalProperties":false}`),
	},
	{
		Name:        "runs_list",
		Description: "List runs in the current project or tenant store, newest first, with optional status and workflow filters.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"},"workflow":{"type":"string"},"limit":{"type":"integer","description":"Maximum runs (default 20, max 200)."}},"additionalProperties":false}`),
	},
}

func ToolsFor(caps Capabilities) []Tool {
	if !caps[CapRunsRead] {
		return nil
	}
	out := append([]Tool(nil), tools...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func ToolNamesFor(caps Capabilities) []string {
	available := ToolsFor(caps)
	out := make([]string, 0, len(available))
	for _, tool := range available {
		out = append(out, tool.Name)
	}
	return out
}

func Call(ctx context.Context, rs store.RunStore, caps Capabilities, name string, raw json.RawMessage) (json.RawMessage, error) {
	if rs == nil {
		return nil, errors.New("runops: run store unavailable")
	}
	if !caps[CapRunsRead] {
		return nil, ErrCapabilityDenied
	}
	switch name {
	case "run_get":
		return callRunGet(ctx, rs, raw)
	case "run_events":
		return callRunEvents(ctx, rs, raw)
	case "runs_list":
		return callRunsList(ctx, rs, raw)
	default:
		return nil, fmt.Errorf("runops: unknown tool %q", name)
	}
}

type runDiagnosis struct {
	ID           string          `json:"id"`
	Status       store.RunStatus `json:"status"`
	WorkflowName string          `json:"workflow_name,omitempty"`
	Resumable    bool            `json:"resumable,omitempty"`
	FailingNode  string          `json:"failing_node,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`

	// ParentRunID and Ancestors carry the run's LINEAGE, nearest parent
	// first. A subbot child is only meaningful to whoever is waiting for its
	// output, so a diagnosis that stops at the run itself is a diagnosis of
	// half the situation.
	ParentRunID string        `json:"parent_run_id,omitempty"`
	Ancestors   []runAncestor `json:"ancestors,omitempty"`

	// Orphaned is the fact Resumable cannot express. Resumable answers "will
	// the store accept a resume of THIS run" — which is true of an abandoned
	// subbot whose whole ancestry died days ago. Orphaned answers the
	// question a caller actually has: is anybody still waiting for what this
	// run produces? When it is true, resuming this run alone completes work
	// nothing will collect; the run to resume is Root.
	//
	// Purely mechanical: a subbot's output is awaited by its parent, so the
	// chain must contain a run that is running or queued for the work to
	// reach anyone.
	// NOT omitempty, unlike its neighbours. On a ROOT run every other lineage
	// field is legitimately absent, which made "this run has no parent"
	// indistinguishable from "this server does not report lineage" — and a
	// caller careful enough to notice the difference had no way to resolve
	// it, so the rigorous bot stalled while the careless one guessed. An
	// always-present `orphaned` is the witness that lineage WAS evaluated:
	// present + no parent_run_id = a root, definitively.
	Orphaned bool   `json:"orphaned"`
	RootID   string `json:"root_run_id,omitempty"`
}

type runAncestor struct {
	ID           string          `json:"id"`
	Status       store.RunStatus `json:"status"`
	WorkflowName string          `json:"workflow_name,omitempty"`
}

// maxAncestorWalk bounds the lineage walk. A cycle cannot occur through
// parent_run_id today, but a bounded loop over untrusted stored ids costs
// nothing and cannot hang a bot's turn.
const maxAncestorWalk = 16

// resolveLineage walks parent_run_id upwards. Failures are SILENT by design:
// an ancestor the caller cannot read (deleted, another tenant) must degrade
// the diagnosis to "no lineage known", never fail the whole run_get — the
// status and failing node are still worth returning. The consequence is that
// Orphaned is only ever set on a chain that was fully readable.
func resolveLineage(ctx context.Context, rs store.RunStore, run *store.Run) ([]runAncestor, bool, string) {
	if run.ParentRunID == "" {
		return nil, false, ""
	}
	var (
		chain     []runAncestor
		seen      = map[string]bool{run.ID: true}
		cur       = run.ParentRunID
		rootID    string
		anyLive   bool
		fullyRead = true
	)
	for i := 0; i < maxAncestorWalk && cur != "" && !seen[cur]; i++ {
		seen[cur] = true
		parent, err := rs.LoadRun(ctx, cur)
		if err != nil || parent == nil {
			fullyRead = false
			break
		}
		chain = append(chain, runAncestor{
			ID: parent.ID, Status: parent.Status,
			WorkflowName: truncate(parent.WorkflowName, 240),
		})
		if parent.Status == store.RunStatusRunning || parent.Status == store.RunStatusQueued {
			anyLive = true
		}
		rootID = parent.ID
		cur = parent.ParentRunID
	}
	return chain, fullyRead && len(chain) > 0 && !anyLive, rootID
}

func callRunGet(ctx context.Context, rs store.RunStore, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		RunID string `json:"run_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.RunID) == "" {
		return nil, errors.New("run_id is required")
	}
	run, err := rs.LoadRun(ctx, args.RunID)
	if err != nil {
		return nil, publicStoreError("run", err)
	}
	out := runDiagnosis{
		ID:           run.ID,
		Status:       run.Status,
		WorkflowName: truncate(run.WorkflowName, 240),
		Resumable:    run.Status.IsResumable(),
		Error:        truncate(run.Error, 4000),
		CreatedAt:    run.CreatedAt,
		UpdatedAt:    run.UpdatedAt,
	}
	if run.Checkpoint != nil {
		out.FailingNode = truncate(run.Checkpoint.NodeID, 240)
	}
	out.ParentRunID = run.ParentRunID
	out.Ancestors, out.Orphaned, out.RootID = resolveLineage(ctx, rs, run)
	if err := rs.ScanEvents(ctx, run.ID, func(evt *store.Event) bool {
		if evt == nil || evt.Type != store.EventRunFailed {
			return true
		}
		if evt.NodeID != "" {
			out.FailingNode = truncate(evt.NodeID, 240)
		}
		if code, ok := evt.Data["code"].(string); ok {
			out.ErrorCode = truncate(code, 80)
		}
		return true
	}); err != nil {
		return nil, publicStoreError("run events", err)
	}
	return json.Marshal(out)
}

func callRunEvents(ctx context.Context, rs store.RunStore, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		RunID string `json:"run_id"`
		Since int64  `json:"since"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.RunID) == "" {
		return nil, errors.New("run_id is required")
	}
	if args.Since < 0 {
		return nil, errors.New("since must be non-negative")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 250 {
		limit = 250
	}
	events, err := rs.LoadEventsRange(ctx, args.RunID, args.Since, 0, limit)
	if err != nil {
		return nil, publicStoreError("run events", err)
	}
	projected := make([]runEvent, 0, len(events))
	const maxResponseBytes = 192 * 1024
	projectedBytes := 0
	for _, event := range events {
		if event == nil {
			continue
		}
		item := projectRunEvent(event)
		encoded, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if len(projected) > 0 && projectedBytes+len(encoded) > maxResponseBytes {
			break
		}
		projected = append(projected, item)
		projectedBytes += len(encoded)
	}
	next := args.Since
	if len(projected) > 0 {
		next = projected[len(projected)-1].Seq + 1
	}
	return json.Marshal(map[string]any{
		"events":    projected,
		"next_seq":  next,
		"truncated": len(projected) < len(events) || len(events) == limit,
	})
}

// runEvent is deliberately not store.Event. The durable event payload can
// contain operator messages, model prompts, run inputs and complete tool/node
// outputs. runs.read is a diagnostic capability, not a generic event export,
// so only a small allow-list of scalar lifecycle facts crosses the boundary.
type runEvent struct {
	Seq       int64           `json:"seq"`
	Timestamp time.Time       `json:"timestamp"`
	Type      store.EventType `json:"type"`
	BranchID  string          `json:"branch_id,omitempty"`
	NodeID    string          `json:"node_id,omitempty"`
	Data      map[string]any  `json:"data,omitempty"`
}

var diagnosticEventKeys = map[string]bool{
	"applied": true, "attempt": true, "axis": true, "budget_pct": true,
	"child_run_id": true, "code": true, "command": true, "delay_ms": true,
	"delta": true, "duration_ms": true, "effective": true, "error": true,
	"expression": true, "files_created_removed": true, "files_modified_restored": true,
	"files_restore_scope": true, "files_reverted": true, "from": true,
	"from_node": true, "input_size": true, "iteration": true, "kind": true,
	"max": true, "max_attempts": true, "mode": true, "orphaned_child_runs": true,
	"output_size": true, "pivot": true, "policy": true, "postcondition_met": true,
	"reason": true, "recoverable": true, "reset_source": true, "resumable": true,
	"retry_after": true, "rung": true, "runtime_code": true, "source": true,
	"status": true, "subbot": true, "target": true, "to": true, "to_node": true,
	"tool": true,
}

func projectRunEvent(event *store.Event) runEvent {
	out := runEvent{
		Seq: event.Seq, Timestamp: event.Timestamp, Type: event.Type,
		BranchID: truncate(event.BranchID, 160), NodeID: truncate(event.NodeID, 240),
	}
	for key, value := range event.Data {
		if !diagnosticEventKeys[key] {
			continue
		}
		if projected, ok := projectDiagnosticValue(value); ok {
			if out.Data == nil {
				out.Data = map[string]any{}
			}
			out.Data[key] = projected
		}
	}
	return out
}

func projectDiagnosticValue(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return truncate(typed, 600), true
	case bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return typed, true
	case []string:
		return projectDiagnosticStrings(typed), true
	case []any:
		out := make([]any, 0, min(len(typed), 20))
		for _, item := range typed {
			if len(out) == 20 {
				break
			}
			if projected, ok := projectDiagnosticValue(item); ok {
				out = append(out, projected)
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func projectDiagnosticStrings(values []string) []string {
	limit := min(len(values), 20)
	out := make([]string, 0, limit)
	for _, value := range values[:limit] {
		out = append(out, truncate(value, 240))
	}
	return out
}

type runSummary struct {
	ID           string          `json:"id"`
	Status       store.RunStatus `json:"status"`
	WorkflowName string          `json:"workflow_name,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	Error        string          `json:"error,omitempty"`
}

func callRunsList(ctx context.Context, rs store.RunStore, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Status   string `json:"status"`
		Workflow string `json:"workflow"`
		Limit    int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	ids, err := rs.ListRuns(ctx)
	if err != nil {
		return nil, publicStoreError("run list", err)
	}
	items := make([]runSummary, 0, len(ids))
	for _, id := range ids {
		run, loadErr := rs.LoadRun(ctx, id)
		if loadErr != nil {
			continue
		}
		if args.Status != "" && string(run.Status) != args.Status {
			continue
		}
		if args.Workflow != "" && run.WorkflowName != args.Workflow {
			continue
		}
		items = append(items, runSummary{
			ID:           run.ID,
			Status:       run.Status,
			WorkflowName: truncate(run.WorkflowName, 240),
			CreatedAt:    run.CreatedAt,
			UpdatedAt:    run.UpdatedAt,
			Error:        truncate(run.Error, 1200),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	total := len(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return json.Marshal(map[string]any{"runs": items, "total": total, "truncated": total > limit})
}

// publicStoreError is the MCP/tool trust boundary. Filesystem stores wrap
// ordinary failures with their absolute run.json/events.jsonl paths, while
// remote stores may include connection details. Those diagnostics belong in
// host logs, never in a model-visible tool error. Keep only the distinction a
// debugging assistant can act on.
func publicStoreError(operation string, err error) error {
	switch {
	case errors.Is(err, store.ErrRunNotFound), errors.Is(err, store.ErrRunDeleted), errors.Is(err, os.ErrNotExist):
		return errors.New("run not found")
	case errors.Is(err, context.Canceled):
		return errors.New("request cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("request timed out")
	default:
		return fmt.Errorf("%s unavailable", operation)
	}
}

func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func truncate(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}
