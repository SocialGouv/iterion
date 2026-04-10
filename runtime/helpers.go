package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/ir"
	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/model"
	"github.com/SocialGouv/iterion/store"
)

// ---------------------------------------------------------------------------
// Node field accessors — thin wrappers over ir.Node* exported helpers.
// ---------------------------------------------------------------------------

var (
	nodePublish     = ir.NodePublish
	nodeAwaitMode   = ir.NodeAwaitMode
	nodeInteraction = ir.NodeInteraction
	isTerminalNode  = ir.IsTerminalNode
)

// ---------------------------------------------------------------------------
// Event emission
// ---------------------------------------------------------------------------

// emit is a convenience wrapper for appending an event.
func (e *Engine) emit(runID string, typ store.EventType, nodeID string, data map[string]interface{}) error {
	_, err := e.store.AppendEvent(runID, store.Event{
		Type:   typ,
		NodeID: nodeID,
		Data:   data,
	})
	if err != nil {
		return fmt.Errorf("runtime: emit %s: %w", typ, err)
	}
	e.logEvent(typ, nodeID, "", data)
	return nil
}

// emitBranch appends an event with a branch ID.
func (e *Engine) emitBranch(runID, branchID string, typ store.EventType, nodeID string, data map[string]interface{}) error {
	_, err := e.store.AppendEvent(runID, store.Event{
		Type:     typ,
		BranchID: branchID,
		NodeID:   nodeID,
		Data:     data,
	})
	if err != nil {
		return fmt.Errorf("runtime: emit %s (branch %s): %w", typ, branchID, err)
	}
	e.logEvent(typ, nodeID, branchID, data)
	return nil
}

// logEvent writes a human-friendly console log for a given event type.
func (e *Engine) logEvent(typ store.EventType, nodeID, branchID string, data map[string]interface{}) {
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

// ---------------------------------------------------------------------------
// Run failure
// ---------------------------------------------------------------------------

// failRun marks a run as failed and emits the run_failed event.
// If reason is already a RuntimeError it preserves the code and hint.
func (e *Engine) failRun(runID, nodeID, reason string) error {
	return e.failRunWithCode(runID, nodeID, reason, ErrCodeExecutionFailed, "")
}

// failRunErr marks a run as failed, preserving a structured error if present.
// Store/event errors are propagated so callers know whether the failure was persisted.
func (e *Engine) failRunErr(runID, nodeID string, origErr error) error {
	var rtErr *RuntimeError
	if errors.As(origErr, &rtErr) {
		if storeErr := e.store.UpdateRunStatus(runID, store.RunStatusFailed, rtErr.Message); storeErr != nil {
			log.Printf("runtime: failed to persist run failure status: %v", storeErr)
			return fmt.Errorf("runtime: node %q failed (%s) and could not persist failure: %w", nodeID, rtErr.Message, storeErr)
		}
		if err := e.emit(runID, store.EventRunFailed, nodeID, map[string]interface{}{
			"error": rtErr.Message,
			"code":  string(rtErr.Code),
		}); err != nil {
			log.Printf("runtime: failed to emit run_failed event: %v", err)
		}
		if rtErr.NodeID == "" {
			rtErr.NodeID = nodeID
		}
		return rtErr
	}
	return e.failRun(runID, nodeID, origErr.Error())
}

// failRunWithCode marks a run as failed and returns a structured RuntimeError.
// If the store update fails, the store error is returned instead of the runtime
// error so callers know the failure state was not persisted.
func (e *Engine) failRunWithCode(runID, nodeID, reason string, code ErrorCode, hint string) error {
	if storeErr := e.store.UpdateRunStatus(runID, store.RunStatusFailed, reason); storeErr != nil {
		log.Printf("runtime: failed to persist run failure status: %v", storeErr)
		return fmt.Errorf("runtime: node %q failed (%s) and could not persist failure: %w", nodeID, reason, storeErr)
	}
	if err := e.emit(runID, store.EventRunFailed, nodeID, map[string]interface{}{
		"error": reason,
		"code":  string(code),
	}); err != nil {
		log.Printf("runtime: failed to emit run_failed event: %v", err)
	}
	return &RuntimeError{
		Code:    code,
		Message: reason,
		NodeID:  nodeID,
		Hint:    hint,
	}
}

// ---------------------------------------------------------------------------
// Resumable failure — checkpoint-aware variants
// ---------------------------------------------------------------------------

// buildCheckpoint creates a Checkpoint from the current runState.
func buildCheckpoint(rs *runState, nodeID string) *store.Checkpoint {
	return &store.Checkpoint{
		NodeID:             nodeID,
		Outputs:            rs.outputs,
		LoopCounters:       rs.loopCounters,
		RoundRobinCounters: rs.roundRobinCounters,
		ArtifactVersions:   rs.artifactVersions,
		Vars:               rs.vars,
	}
}

// failRunWithCheckpoint marks a run as failed_resumable with a checkpoint,
// enabling resume from the last completed node. Falls back to a regular
// (non-resumable) failure if the checkpoint write fails.
func (e *Engine) failRunWithCheckpoint(rs *runState, nodeID, reason string) error {
	cp := buildCheckpoint(rs, nodeID)
	if storeErr := e.store.FailRunResumable(rs.runID, cp, reason); storeErr != nil {
		log.Printf("runtime: failed to persist resumable failure: %v", storeErr)
		return e.failRun(rs.runID, nodeID, reason)
	}
	if err := e.emit(rs.runID, store.EventRunFailed, nodeID, map[string]interface{}{
		"error":     reason,
		"code":      string(ErrCodeExecutionFailed),
		"resumable": true,
	}); err != nil {
		log.Printf("runtime: failed to emit run_failed event: %v", err)
	}
	return &RuntimeError{
		Code:    ErrCodeExecutionFailed,
		Message: reason,
		NodeID:  nodeID,
	}
}

// failRunErrWithCheckpoint is the checkpoint-aware variant of failRunErr.
func (e *Engine) failRunErrWithCheckpoint(rs *runState, nodeID string, origErr error) error {
	var rtErr *RuntimeError
	if errors.As(origErr, &rtErr) {
		cp := buildCheckpoint(rs, nodeID)
		if storeErr := e.store.FailRunResumable(rs.runID, cp, rtErr.Message); storeErr != nil {
			log.Printf("runtime: failed to persist resumable failure: %v", storeErr)
			return e.failRunErr(rs.runID, nodeID, origErr)
		}
		if err := e.emit(rs.runID, store.EventRunFailed, nodeID, map[string]interface{}{
			"error":     rtErr.Message,
			"code":      string(rtErr.Code),
			"resumable": true,
		}); err != nil {
			log.Printf("runtime: failed to emit run_failed event: %v", err)
		}
		if rtErr.NodeID == "" {
			rtErr.NodeID = nodeID
		}
		return rtErr
	}
	return e.failRunWithCheckpoint(rs, nodeID, origErr.Error())
}

// ---------------------------------------------------------------------------
// Context handling
// ---------------------------------------------------------------------------

// handleContextDoneWithCheckpoint handles context cancellation or deadline
// exceeded, preserving the checkpoint so the run can be resumed.
func (e *Engine) handleContextDoneWithCheckpoint(rs *runState, nodeID string, ctxErr error) error {
	if errors.Is(ctxErr, context.Canceled) {
		// Save checkpoint so the cancelled run can be resumed.
		cp := buildCheckpoint(rs, nodeID)
		if err := e.store.SaveCheckpoint(rs.runID, cp); err != nil {
			log.Printf("runtime: failed to save checkpoint on cancellation: %v", err)
		}
		// Keep "cancelled" status but with checkpoint preserved.
		if err := e.store.UpdateRunStatus(rs.runID, store.RunStatusCancelled, "run cancelled"); err != nil {
			log.Printf("runtime: failed to persist cancellation status: %v", err)
		}
		if err := e.emit(rs.runID, store.EventRunCancelled, nodeID, map[string]interface{}{
			"reason": "context cancelled",
		}); err != nil {
			log.Printf("runtime: failed to emit run_cancelled event: %v", err)
		}
		return fmt.Errorf("%w: interrupted at node %s", ErrRunCancelled, nodeID)
	}
	// context.DeadlineExceeded → save checkpoint and mark as resumable.
	reason := fmt.Sprintf("timeout: %s", ctxErr.Error())
	return e.failRunWithCheckpoint(rs, nodeID, reason)
}

// wrapContextErr wraps a context error for branch-level reporting.
func (e *Engine) wrapContextErr(ctxErr error) error {
	if errors.Is(ctxErr, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrRunCancelled, ctxErr)
	}
	return ctxErr
}

// ---------------------------------------------------------------------------
// Budget helpers
// ---------------------------------------------------------------------------

// checkBudgetBeforeExec checks budget limits before a node runs.
// It enforces both hard exceeded (100%) and hard limited (90%) thresholds.
func (e *Engine) checkBudgetBeforeExec(rs *runState, nodeID string) error {
	if rs.budget == nil {
		return nil
	}
	checks := rs.budget.Check()

	// Hard exceeded (100%+).
	if exc := findExceeded(checks); exc != nil {
		_ = e.emit(rs.runID, store.EventBudgetExceeded, nodeID, map[string]interface{}{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		})
		return e.failRunErrWithCheckpoint(rs, nodeID, &RuntimeError{
			Code:    ErrCodeBudgetExceeded,
			Message: fmt.Sprintf("budget exceeded: %s (%.0f/%.0f)", exc.dimension, exc.used, exc.limit),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("increase the %s budget or optimize the workflow", exc.dimension),
		})
	}

	// Hard limit (90%+) — refuse new node executions to prevent concurrent overage.
	if hl := findHardLimited(checks); hl != nil {
		_ = e.emit(rs.runID, store.EventBudgetExceeded, nodeID, map[string]interface{}{
			"dimension":  hl.dimension,
			"used":       hl.used,
			"limit":      hl.limit,
			"hard_limit": true,
		})
		return e.failRunErrWithCheckpoint(rs, nodeID, &RuntimeError{
			Code:    ErrCodeBudgetExceeded,
			Message: fmt.Sprintf("budget hard limit reached: %s at %.0f%% (%.0f/%.0f)", hl.dimension, (hl.used/hl.limit)*100, hl.used, hl.limit),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("increase the %s budget or optimize the workflow; new executions are blocked at 90%% to prevent concurrent overage", hl.dimension),
		})
	}

	return nil
}

// recordAndCheckBudget records usage from a node execution and emits
// budget_warning / budget_exceeded events as needed.
func (e *Engine) recordAndCheckBudget(rs *runState, nodeID string, output map[string]interface{}) error {
	if rs.budget == nil {
		return nil
	}

	tokens, costUSD := extractUsage(output)
	checks := rs.budget.RecordUsage(tokens, costUSD)

	// Emit warnings.
	for _, w := range findWarnings(checks) {
		_ = e.emit(rs.runID, store.EventBudgetWarning, nodeID, map[string]interface{}{
			"dimension": w.dimension,
			"used":      w.used,
			"limit":     w.limit,
		})
	}

	// Fail on exceeded.
	if exc := findExceeded(checks); exc != nil {
		_ = e.emit(rs.runID, store.EventBudgetExceeded, nodeID, map[string]interface{}{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		})
		return e.failRunErrWithCheckpoint(rs, nodeID, &RuntimeError{
			Code:    ErrCodeBudgetExceeded,
			Message: fmt.Sprintf("budget exceeded: %s (%.0f/%.0f)", exc.dimension, exc.used, exc.limit),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("increase the %s budget or optimize the workflow", exc.dimension),
		})
	}

	return nil
}

// ---------------------------------------------------------------------------
// Schema validation
// ---------------------------------------------------------------------------

// validateNodeOutput checks that the node's output conforms to its declared
// output schema. Returns nil if validation is disabled, the node has no
// output schema, or the output is valid.
func (e *Engine) validateNodeOutput(nodeID string, node ir.Node, output map[string]interface{}) error {
	if !e.validateOutputs {
		return nil
	}
	schemaName := ir.NodeOutputSchema(node)
	if schemaName == "" {
		return nil
	}
	schema, ok := e.workflow.Schemas[schemaName]
	if !ok {
		return nil // schema not found; compile-time validation covers this
	}
	// ValidateOutput only checks declared schema fields — extra keys
	// (including _-prefixed metadata) are silently ignored.
	if err := model.ValidateOutput(output, schema); err != nil {
		return &RuntimeError{
			Code:    ErrCodeSchemaValidation,
			Message: fmt.Sprintf("node %q output does not match schema %q: %v", nodeID, schemaName, err),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("ensure node %q produces output conforming to schema %q", nodeID, schemaName),
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Output utilities
// ---------------------------------------------------------------------------

// extractUsage reads conventional _tokens and _cost_usd keys from a node
// output. Returns zeros if absent.
func extractUsage(output map[string]interface{}) (tokens int, costUSD float64) {
	if v, ok := output["_tokens"]; ok {
		switch t := v.(type) {
		case int:
			tokens = t
		case float64:
			tokens = int(t)
		case int64:
			tokens = int(t)
		}
	}
	if v, ok := output["_cost_usd"]; ok {
		switch t := v.(type) {
		case float64:
			costUSD = t
		case int:
			costUSD = float64(t)
		}
	}
	return
}

// buildNodeFinishedData builds the data payload for a node_finished event,
// including usage metrics (_tokens, _cost_usd) and a snapshot of the output.
func buildNodeFinishedData(output map[string]interface{}) map[string]interface{} {
	if output == nil {
		return nil
	}
	data := map[string]interface{}{
		"output": output,
	}
	if v, ok := output["_tokens"]; ok {
		data["_tokens"] = v
	}
	if v, ok := output["_cost_usd"]; ok {
		data["_cost_usd"] = v
	}
	return data
}

// formatOutputPreview builds a human-readable single-line summary of a
// node_finished event's data. It returns an empty string when there is
// nothing meaningful to display.
func formatOutputPreview(data map[string]interface{}) string {
	if data == nil {
		return ""
	}

	// Regular nodes wrap output under data["output"]; router events put
	// fields like selected_route/reasoning directly in data.
	output, ok := data["output"].(map[string]interface{})
	if !ok {
		output = data
	}

	// Collect user-visible fields (skip internal _-prefixed keys).
	type kv struct {
		key string
		val interface{}
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
func formatFieldValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return truncatePreview(val, 200)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []interface{}:
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

// ---------------------------------------------------------------------------
// Edge evaluation
// ---------------------------------------------------------------------------

// evaluateEdges walks the workflow edges originating from fromNodeID and returns
// the first conditional match (or the first unconditional fallback). It returns
// nil when no edge matches. The logPrefix is included in warning messages.
// This variant does NOT check loop counters — use evaluateEdgesWithLoops for
// loop-aware selection.
func (e *Engine) evaluateEdges(fromNodeID, logPrefix string, output map[string]interface{}) *ir.Edge {
	var unconditional *ir.Edge

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}
		if edge.Condition == "" {
			if unconditional == nil {
				unconditional = edge
			}
			continue
		}
		val, ok := output[edge.Condition]
		if !ok {
			continue
		}
		boolVal, isBool := val.(bool)
		if !isBool {
			log.Printf("runtime: %s: node %q: condition field %q is %T, expected bool — edge to %q skipped",
				logPrefix, fromNodeID, edge.Condition, val, edge.To)
			continue
		}
		if edge.Negated {
			boolVal = !boolVal
		}
		if boolVal {
			return edge
		}
	}

	return unconditional
}

// evaluateEdgesWithLoops is like evaluateEdges but skips edges whose loop
// counter is exhausted. This enables fallback patterns: when a fix_loop edge
// is exhausted, the next matching edge (e.g., outer_loop or unconditional)
// is selected instead of producing a fatal LOOP_EXHAUSTED error.
func (e *Engine) evaluateEdgesWithLoops(fromNodeID, logPrefix string, output map[string]interface{}, loopCounters map[string]int) *ir.Edge {
	var unconditional *ir.Edge

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}

		// Skip edges whose loop is exhausted.
		if edge.LoopName != "" {
			loop, ok := e.workflow.Loops[edge.LoopName]
			if ok && loopCounters[edge.LoopName] >= loop.MaxIterations {
				log.Printf("runtime: %s: node %q: edge to %q skipped — loop %q exhausted (%d/%d)",
					logPrefix, fromNodeID, edge.To, edge.LoopName, loopCounters[edge.LoopName], loop.MaxIterations)
				continue
			}
		}

		if edge.Condition == "" {
			if unconditional == nil {
				unconditional = edge
			}
			continue
		}
		val, ok := output[edge.Condition]
		if !ok {
			continue
		}
		boolVal, isBool := val.(bool)
		if !isBool {
			log.Printf("runtime: %s: node %q: condition field %q is %T, expected bool — edge to %q skipped",
				logPrefix, fromNodeID, edge.Condition, val, edge.To)
			continue
		}
		if edge.Negated {
			boolVal = !boolVal
		}
		if boolVal {
			return edge
		}
	}

	return unconditional
}
