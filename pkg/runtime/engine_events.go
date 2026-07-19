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
func (e *Engine) emitBranch(ctx context.Context, runID, branchID string, typ store.EventType, nodeID string, data map[string]any) error {
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
		l.Warn("Budget warning: %s", nodeID)
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
