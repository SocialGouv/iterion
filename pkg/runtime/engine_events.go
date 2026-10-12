package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Event emission
// ---------------------------------------------------------------------------

// emit is a convenience wrapper for appending an event with no branch ID.
func (e *Engine) emit(ctx context.Context, runID string, typ store.EventType, nodeID string, data map[string]any) error {
	return e.emitBranch(ctx, runID, "", typ, nodeID, data)
}

// emitBranch appends an event, optionally tagged with a branch ID. A blank
// branchID is the non-branch case (what emit forwards) and keeps the
// branch-free error message.
//
// Events are an observational sink, like node_finished: when the executor
// implements SecretScrubber, the event and its console line carry the
// redacted copy. A pause's questions, an author's rendered instructions or
// a node error may quote a secret the run was launched with; the data the
// engine reads back — checkpoint, interaction records — keeps the values.
func (e *Engine) emitBranch(ctx context.Context, runID, branchID string, typ store.EventType, nodeID string, data map[string]any) error {
	// The recorded-answers event is scrubbed narrowly, in answersEventData:
	// its interaction_id is the key the studio and the interaction records
	// flip on, and it must survive even when it happens to hold a
	// registered value (an id is engine data, not what the model wrote).
	if scrubber, ok := e.executor.(SecretScrubber); ok && data != nil && typ != store.EventHumanAnswersRecorded {
		data = scrubber.ScrubOutput(data)
	}
	if typ == store.EventNodeFinished && nodeID != "" {
		data = withFinishStamps(data, nodeMayWriteScratch(e.workflow, nodeID), onCycle(e.workflow, nodeID))
	}
	evt := store.Event{
		Type:     typ,
		BranchID: branchID,
		NodeID:   nodeID,
		Data:     data,
	}
	persisted, err := e.store.AppendEvent(ctx, runID, evt)
	if err != nil {
		if branchID != "" {
			return fmt.Errorf("runtime: emit %s (branch %s): %w", typ, branchID, err)
		}
		return fmt.Errorf("runtime: emit %s: %w", typ, err)
	}
	if e.onEvent != nil && persisted != nil {
		e.onEvent(*persisted)
	}
	e.logEvent(typ, nodeID, branchID, data)
	return nil
}

// withFinishStamps returns data with the facts a resume reads from a finish
// — whether the executing workflow runs the node in the sandbox
// (nodeFinishedInSandbox), whether the node lies on a cycle there
// (nodeFinishedOnCycle) — leaving the caller's map untouched.
func withFinishStamps(data map[string]any, inSandbox, cyclic bool) map[string]any {
	out := make(map[string]any, len(data)+2)
	for k, v := range data {
		out[k] = v
	}
	out[nodeFinishedInSandbox] = inSandbox
	out[nodeFinishedOnCycle] = cyclic
	return out
}

// OnlyFinishStamps reports whether a node_finished event's data holds
// nothing but the facts the engine stamps on every finish: the finish of a
// node that produced nothing — a terminal node, a router, a gate.
func OnlyFinishStamps(data map[string]any) bool {
	for k := range data {
		if k != nodeFinishedInSandbox && k != nodeFinishedOnCycle {
			return false
		}
	}
	return true
}

// logEvent writes a human-friendly console log for a given event type.
func (e *Engine) logEvent(typ store.EventType, nodeID, branchID string, data map[string]any) {
	l := e.logger
	if l == nil {
		return
	}

	prefix := nodeID
	if branchID != "" {
		prefix = branchID + "/" + nodeID
	}

	switch typ {
	case store.EventRunStarted:
		l.Logf(iterlog.LevelInfo, "🚀", "Run started: %s", e.workflow.Name)
	case store.EventRunFinished:
		l.Logf(iterlog.LevelInfo, "✅", "Run finished")
	case store.EventRunFailed:
		reason := ""
		if data != nil {
			if r, ok := data["error"].(string); ok {
				reason = r
			}
		}
		l.Error("Run failed: %s", reason)
	case store.EventRunCancelled:
		l.Error("Run cancelled")
	case store.EventNodeStarted:
		kind := ""
		if data != nil {
			if k, ok := data["kind"].(string); ok {
				kind = k
			}
		}
		l.Logf(iterlog.LevelInfo, "📍", "Node started: %s [%s]", prefix, kind)
	case store.EventNodeFinished:
		tokens := ""
		cost := ""
		if data != nil {
			if t, ok := data["_tokens"]; ok {
				tokens = fmt.Sprintf(", %v tokens", t)
			}
			if c, ok := data["_cost_usd"]; ok {
				if f, ok := c.(float64); ok && f > 0 {
					cost = fmt.Sprintf(", $%.4f", f)
				}
			}
		}
		l.Logf(iterlog.LevelInfo, "✅", "Node finished: %s%s%s", prefix, tokens, cost)
		if data != nil {
			if preview := formatOutputPreview(data); preview != "" {
				l.LogBlock(iterlog.LevelInfo, "📋",
					fmt.Sprintf("Output [%s]:", prefix), preview)
			}
		}
	case store.EventEdgeSelected:
		to := ""
		cond := ""
		if data != nil {
			if t, ok := data["to"].(string); ok {
				to = t
			}
			if c, ok := data["condition"].(string); ok {
				cond = c
			}
		}
		if cond != "" {
			l.Logf(iterlog.LevelInfo, "➡️ ", "Edge: %s → %s (condition: %s)", nodeID, to, cond)
		} else {
			l.Logf(iterlog.LevelInfo, "➡️ ", "Edge: %s → %s", nodeID, to)
		}
	case store.EventBranchStarted:
		l.Logf(iterlog.LevelInfo, "🔀", "Branch started: %s", branchID)
	case store.EventJoinReady:
		l.Logf(iterlog.LevelInfo, "🔗", "Join ready: %s", nodeID)
	case store.EventArtifactWritten:
		l.Logf(iterlog.LevelInfo, "💾", "Artifact written: %s", nodeID)
	case store.EventHumanInputRequested:
		l.Logf(iterlog.LevelInfo, "👤", "Human input requested: %s", nodeID)
	case store.EventRunPaused:
		l.Logf(iterlog.LevelInfo, "⏸️ ", "Run paused (waiting for human input)")
	case store.EventRunResumed:
		l.Logf(iterlog.LevelInfo, "▶️ ", "Run resumed")
	case store.EventHumanAnswersRecorded:
		l.Logf(iterlog.LevelInfo, "📝", "Human answers recorded: %s", nodeID)
	case store.EventBudgetWarning:
		// Name the axis and, when the payload explains itself, say why. A
		// bare "Budget warning: <node>" is the one line an operator scrolls
		// past — and cost_usd_unpriced exists precisely to be noticed.
		if dim, ok := data["dimension"].(string); ok && dim != "" {
			if detail, ok := data["detail"].(string); ok && detail != "" {
				l.Warn("Budget warning [%s] at %s: %s", dim, nodeID, detail)
			} else {
				l.Warn("Budget warning [%s]: %s", dim, nodeID)
			}
		} else {
			l.Warn("Budget warning: %s", nodeID)
		}
	case store.EventBudgetExceeded:
		l.Warn("Budget exceeded: %s", nodeID)
	}
}

// formatOutputPreview builds a human-readable single-line summary of a
// node_finished event's data. It returns an empty string when there is
// nothing meaningful to display.
func formatOutputPreview(data map[string]any) string {
	if data == nil {
		return ""
	}

	// Regular nodes wrap output under data["output"]; router events put
	// fields like selected_route/reasoning directly in data.
	output, ok := data["output"].(map[string]any)
	if !ok {
		output = data
	}

	// Collect user-visible fields (skip internal _-prefixed keys).
	type kv struct {
		key string
		val any
	}

	var fields []kv
	for k, v := range output {
		if strings.HasPrefix(k, "_") {
			continue
		}
		fields = append(fields, kv{k, v})
	}
	if len(fields) == 0 {
		return ""
	}

	// Special case: text-only output — show a preview of the text (preserve newlines).
	if len(fields) == 1 && fields[0].key == "text" {
		s, _ := fields[0].val.(string)
		if s == "" {
			return ""
		}
		return iterlog.BlockPreview(s, 1500)
	}

	// Priority ordering for known fields.
	priority := map[string]int{
		"verdict":         0,
		"approved":        1,
		"selected_route":  2,
		"selected_routes": 3,
		"reasoning":       10,
		"feedback":        11,
		"summary":         12,
		"text":            13,
	}
	sort.SliceStable(fields, func(i, j int) bool {
		pi, oki := priority[fields[i].key]
		pj, okj := priority[fields[j].key]
		if oki && okj {
			return pi < pj
		}
		if oki {
			return true
		}
		if okj {
			return false
		}
		return fields[i].key < fields[j].key
	})

	// Format each field as "key: value" — one per line for readability.
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%s: %s", f.key, formatFieldValue(f.val)))
	}

	result := strings.Join(parts, "\n")
	return iterlog.BlockPreview(result, 1500)
}

// formatFieldValue formats a single output field value for display.
func formatFieldValue(v any) string {
	switch val := v.(type) {
	case string:
		return truncatePreview(val, 200)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []any:
		items := make([]string, 0, len(val))
		for _, item := range val {
			s := fmt.Sprintf("%v", item)
			if len(s) > 80 {
				s = s[:80] + "..."
			}
			items = append(items, s)
			if len(items) >= 5 {
				items = append(items, fmt.Sprintf("... (%d total)", len(val)))
				break
			}
		}
		return "[" + strings.Join(items, ", ") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// truncatePreview returns s truncated to maxLen characters, with "..."
// appended if truncated. Newlines are replaced with spaces for single-line display.
func truncatePreview(s string, maxLen int) string {
	// Replace newlines with spaces.
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
