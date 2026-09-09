package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool/privacy"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Schema validation
// ---------------------------------------------------------------------------

// validateNodeOutput checks that the node's output conforms to its declared
// output schema. Returns nil if validation is disabled, the node has no
// output schema, or the output is valid.
func (e *Engine) validateNodeOutput(nodeID string, node ir.Node, output map[string]any) error {
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

const (
	correctionStatusActive       = "active"
	correctionStatusSucceeded    = "succeeded"
	correctionStatusExhausted    = "exhausted"
	correctionStatusUnchanged    = "unchanged"
	correctionStatusSpendBlocked = "spend_blocked"
)

// correctAndValidateNodeOutput applies the optional bounded correction loop
// around schema validation. The original output has not been published when
// this helper runs, so a correction can never replay a downstream side effect.
// The episode is persisted before each correction call and after each result;
// a process restart therefore resumes the same budget instead of starting a
// fresh watcher-triggered loop.
func (e *Engine) correctAndValidateNodeOutput(ctx context.Context, rs *runState, nodeID string, node ir.Node, output map[string]any) (map[string]any, error) {
	validationErr := e.validateNodeOutput(nodeID, node, output)
	if validationErr == nil {
		return output, nil
	}

	corrector, hasPlainCorrector := e.executor.(OutputCorrector)
	usageCorrector, hasUsageCorrector := e.executor.(OutputCorrectorWithUsage)
	correctionStore := store.AsOutputCorrectionStore(e.store)
	if (!hasPlainCorrector && !hasUsageCorrector) || e.outputCorrectionBudget <= 0 || correctionStore == nil {
		return output, validationErr
	}

	ledgerKey, invocationID := e.correctionInvocationIdentity(rs, nodeID)
	inputFingerprint := correctionSemanticFingerprint(output)
	violationFingerprint := correctionFingerprint(validationErr.Error())
	episode, found, loadErr := e.loadCorrectionEpisode(ctx, rs.runID, ledgerKey)
	if loadErr != nil {
		// A transient read failure must not look like an empty ledger and
		// reset an already-consumed correction budget.
		return output, fmt.Errorf("output correction ledger read: %w (original validation: %v)", loadErr, validationErr)
	}
	// The durable invocation identity identifies the episode. Output can change
	// across a retry/resume and therefore must never reset a consumed budget.
	if !found {
		now := time.Now().UTC()
		episode = store.OutputCorrectionEpisode{
			EpisodeID:                ledgerKey,
			InvocationID:             invocationID,
			NodeID:                   nodeID,
			Budget:                   e.outputCorrectionBudget,
			Status:                   correctionStatusActive,
			InputFingerprint:         inputFingerprint,
			LastOutputFingerprint:    inputFingerprint,
			LastViolationFingerprint: violationFingerprint,
			StartedAt:                now,
			UpdatedAt:                now,
		}
	} else if episode.Budget < e.outputCorrectionBudget {
		// A launch may raise the budget, but never lower an already consumed
		// episode's bound. Re-open only a budget-exhausted episode: unchanged
		// output is semantic convergence and an explicit raise must not turn it
		// back into a loop.
		previousBudget := episode.Budget
		episode.Budget = e.outputCorrectionBudget
		if episode.Status == correctionStatusExhausted && episode.Attempts >= previousBudget && episode.Attempts < episode.Budget {
			episode.Status = correctionStatusActive
			episode.LastError = ""
			episode.UpdatedAt = time.Now().UTC()
			e.emitOutputCorrectionEvent(rs, nodeID, episode, "budget_raised")
			if err := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); err != nil {
				return output, fmt.Errorf("output correction budget raise: %w (original validation: %v)", err, validationErr)
			}
		}
	}

	if episode.Status == correctionStatusUnchanged || episode.Status == correctionStatusExhausted {
		return output, validationErr
	}
	if episode.Budget <= 0 {
		return output, validationErr
	}

	current := output
	currentErr := validationErr
	// The original node call is accounted by the caller after this helper
	// returns. A usage-reporting corrector is another paid model call, so refuse
	// to start even the first attempt when that prospective original usage has
	// already reached the same hard boundary that gates ordinary node calls.
	if hasUsageCorrector {
		if spendErr := e.outputCorrectionSpendError(rs, nodeID, current); spendErr != nil {
			episode.Status = correctionStatusSpendBlocked
			episode.LastOutputFingerprint = correctionSemanticFingerprint(current)
			episode.LastViolationFingerprint = correctionFingerprint(currentErr.Error())
			episode.LastError = spendErr.Error()
			episode.UpdatedAt = time.Now().UTC()
			e.emitOutputCorrectionEvent(rs, nodeID, episode, episode.Status)
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
				return current, fmt.Errorf("output correction spend limit reached: %v; ledger: %w", spendErr, persistErr)
			}
			return current, spendErr
		}
	}
	correctionCtx, cancelCorrection, budgetDeadline, deadlineErr := e.outputCorrectionContext(ctx, rs, nodeID)
	if deadlineErr != nil {
		return current, deadlineErr
	}
	defer cancelCorrection()
	for episode.Attempts < episode.Budget {
		episode.Status = correctionStatusActive
		episode.Attempts++
		episode.LastOutputFingerprint = correctionSemanticFingerprint(current)
		episode.LastViolationFingerprint = correctionFingerprint(currentErr.Error())
		episode.LastError = currentErr.Error()
		episode.UpdatedAt = time.Now().UTC()
		e.emitOutputCorrectionEvent(rs, nodeID, episode, "attempt")
		if err := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); err != nil {
			// Losing the durable ledger is safer than pretending an unbounded
			// correction is allowed: surface the original validation failure and
			// let the normal run lifecycle mark it resumable/failed.
			return current, fmt.Errorf("output correction ledger: %w (original validation: %v)", err, currentErr)
		}

		var candidate map[string]any
		var correctionUsage OutputCorrectionUsage
		var correctionErr error
		if hasUsageCorrector {
			candidate, correctionUsage, correctionErr = usageCorrector.CorrectOutputWithUsage(correctionCtx, node, current, currentErr)
		} else {
			candidate, correctionErr = corrector.CorrectOutput(correctionCtx, node, current, currentErr)
		}
		if correctionErr != nil || candidate == nil {
			// Usage is spend even when the correction call fails or returns no
			// candidate. Carry it back so every caller can charge it before
			// surfacing the validation/correction error.
			current = addCorrectionUsage(current, correctionUsage)
			// A context interruption consumes this paid attempt, but it does not
			// consume attempts that were never started. Keep the episode active
			// when budget remains so an operator can raise max_duration and resume
			// instead of finding a permanently terminal ledger.
			interrupted := correctionCtx.Err() != nil
			if interrupted && episode.Attempts < episode.Budget {
				episode.Status = correctionStatusActive
			} else {
				episode.Status = correctionStatusExhausted
			}
			if correctionErr != nil {
				episode.LastError = correctionErr.Error()
			} else {
				episode.LastError = "corrector returned a nil output"
			}
			episode.UpdatedAt = time.Now().UTC()
			e.emitOutputCorrectionEvent(rs, nodeID, episode, episode.Status)
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
				return current, fmt.Errorf("output correction failed: %v; ledger: %w", episode.LastError, persistErr)
			}
			if budgetDeadline && errors.Is(correctionCtx.Err(), context.DeadlineExceeded) {
				return current, outputCorrectionDurationError(nodeID)
			}
			return current, currentErr
		}
		candidate = preserveCorrectionMetadata(current, candidate, correctionUsage)

		candidateErr := e.validateNodeOutput(nodeID, node, candidate)
		candidateFP := correctionSemanticFingerprint(candidate)
		candidateViolationFP := ""
		if candidateErr != nil {
			candidateViolationFP = correctionFingerprint(candidateErr.Error())
		}
		if candidateErr == nil {
			episode.Status = correctionStatusSucceeded
			episode.LastOutputFingerprint = candidateFP
			episode.LastViolationFingerprint = ""
			episode.LastError = ""
			episode.UpdatedAt = time.Now().UTC()
			e.emitOutputCorrectionEvent(rs, nodeID, episode, episode.Status)
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
				return candidate, fmt.Errorf("output correction succeeded but ledger could not be persisted: %w", persistErr)
			}
			return candidate, nil
		}

		// A corrector that returns the exact same invalid payload is a
		// no-progress loop. Stop immediately, even when budget remains.
		if candidateFP == correctionSemanticFingerprint(current) && candidateViolationFP == correctionFingerprint(currentErr.Error()) {
			episode.Status = correctionStatusUnchanged
			episode.LastOutputFingerprint = candidateFP
			episode.LastViolationFingerprint = candidateViolationFP
			episode.LastError = candidateErr.Error()
			episode.UpdatedAt = time.Now().UTC()
			e.emitOutputCorrectionEvent(rs, nodeID, episode, episode.Status)
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
				return candidate, fmt.Errorf("unchanged output correction; ledger: %w", persistErr)
			}
			return candidate, candidateErr
		}
		// The caller records the aggregate node metadata once, after this
		// helper returns. Inspect that prospective total without mutating the
		// shared tracker so another correction call cannot start after token or
		// cost spend has reached the same 90% boundary as any other model call.
		if spendErr := e.outputCorrectionSpendError(rs, nodeID, candidate); spendErr != nil {
			episode.Status = correctionStatusSpendBlocked
			episode.LastOutputFingerprint = candidateFP
			episode.LastViolationFingerprint = candidateViolationFP
			episode.LastError = spendErr.Error()
			episode.UpdatedAt = time.Now().UTC()
			e.emitOutputCorrectionEvent(rs, nodeID, episode, episode.Status)
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
				return candidate, fmt.Errorf("output correction spend limit reached: %v; ledger: %w", spendErr, persistErr)
			}
			return candidate, spendErr
		}

		current = candidate
		currentErr = candidateErr
		episode.LastOutputFingerprint = candidateFP
		episode.LastViolationFingerprint = candidateViolationFP
		episode.LastError = candidateErr.Error()
	}

	episode.Status = correctionStatusExhausted
	episode.UpdatedAt = time.Now().UTC()
	e.emitOutputCorrectionEvent(rs, nodeID, episode, episode.Status)
	if err := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); err != nil {
		return current, fmt.Errorf("output correction budget exhausted; ledger: %w", err)
	}
	return current, currentErr
}

func (e *Engine) outputCorrectionSpendError(rs *runState, nodeID string, output map[string]any) error {
	if rs == nil || rs.budget == nil {
		return nil
	}
	tokens, costUSD := extractUsage(output)
	status := rs.budget.Status()
	type axis struct {
		name        string
		used, limit float64
	}
	for _, candidate := range []axis{
		{name: "tokens", used: float64(status.Tokens + tokens), limit: float64(status.MaxTokens)},
		{name: "cost_usd", used: status.CostUSD + costUSD, limit: status.MaxCostUSD},
	} {
		if candidate.limit <= 0 || candidate.used/candidate.limit < budgetHardThreshold {
			continue
		}
		return &RuntimeError{
			Code:    ErrCodeBudgetExceeded,
			Message: fmt.Sprintf("output correction for node %q reached the %s budget boundary (%.0f/%.0f)", nodeID, candidate.name, candidate.used, candidate.limit),
			NodeID:  nodeID,
			Hint:    fmt.Sprintf("increase the %s budget or reduce correction attempts", candidate.name),
			Cause:   ErrBudgetExceeded,
		}
	}
	return nil
}

func (e *Engine) emitOutputCorrectionEvent(rs *runState, nodeID string, episode store.OutputCorrectionEpisode, phase string) {
	if rs == nil {
		return
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeRecovery, nodeID, map[string]any{
		"strategy": "schema_output_correction",
		"phase":    phase,
		"attempt":  episode.Attempts,
		"budget":   episode.Budget,
		"status":   episode.Status,
	}); err != nil && e.logger != nil {
		e.logger.Warn("output correction: failed to emit event for node %q: %v", nodeID, err)
	}
}

// preserveCorrectionMetadata carries load-bearing underscore fields from the
// original node call onto a corrected payload. A corrector can override a
// field deliberately; absent fields are inherited. When usage is reported,
// the correction call is added to the original totals before normal budget
// accounting runs.
func preserveCorrectionMetadata(original, candidate map[string]any, usage OutputCorrectionUsage) map[string]any {
	if candidate == nil {
		return nil
	}
	out := make(map[string]any, len(candidate)+4)
	for key, value := range candidate {
		out[key] = value
	}
	for key, value := range original {
		if !strings.HasPrefix(key, "_") {
			continue
		}
		if _, exists := out[key]; !exists {
			out[key] = value
		}
	}
	if usage.Tokens > 0 {
		previous, _ := extractUsage(original)
		out["_tokens"] = previous + usage.Tokens
	}
	if usage.CostUSD > 0 {
		_, previous := extractUsage(original)
		out["_cost_usd"] = previous + usage.CostUSD
	}
	return out
}

func addCorrectionUsage(output map[string]any, usage OutputCorrectionUsage) map[string]any {
	if usage.Tokens <= 0 && usage.CostUSD <= 0 {
		return output
	}
	out := make(map[string]any, len(output)+2)
	for key, value := range output {
		out[key] = value
	}
	previousTokens, previousCost := extractUsage(output)
	if usage.Tokens > 0 {
		out["_tokens"] = previousTokens + usage.Tokens
	}
	if usage.CostUSD > 0 {
		out["_cost_usd"] = previousCost + usage.CostUSD
	}
	return out
}

// correctionSemanticFingerprint ignores runtime metadata. Accounting fields
// change after every model call and must not disguise an unchanged invalid
// payload as progress.
func correctionSemanticFingerprint(output map[string]any) string {
	semantic := make(map[string]any, len(output))
	for key, value := range output {
		if strings.HasPrefix(key, "_") {
			continue
		}
		semantic[key] = value
	}
	return correctionFingerprint(semantic)
}

// correctionInvocationIdentity returns a durable per-invocation ledger key.
// A root, non-loop node keeps its historical node-id key for compatibility;
// loop/foreach iterations, fan-out branches, and Mongo-unsafe node IDs use a
// stable hash so distinct executions cannot reset or share one another's
// budget.
func (e *Engine) correctionInvocationIdentity(rs *runState, nodeID string) (ledgerKey, invocationID string) {
	iterationPath := ""
	scope := ""
	if rs != nil {
		iterationPath = e.currentCorrectionIterationPath(nodeID, runStateIterationCounters(rs))
		scope = rs.correctionScope
	}
	invocationID = fmt.Sprintf("node=%s;branch=%s;loops=%s", nodeID, scope, iterationPath)
	if scope == "" && iterationPath == "" && !strings.Contains(nodeID, ".") && !strings.HasPrefix(nodeID, "$") {
		return nodeID, invocationID
	}
	return "inv_" + correctionFingerprint(invocationID), invocationID
}

// currentCorrectionIterationPath extends the historical loop path with every
// foreach whose body contains nodeID. Foreach counters live under a namespaced
// key in runState, so a loop and a foreach may safely share the same DSL name.
// Keeping the loop portion byte-for-byte identical preserves existing durable
// ledger identities for workflows that do not use foreach.
func (e *Engine) currentCorrectionIterationPath(nodeID string, iterationCounters map[string]int) string {
	parts := make([]string, 0, 2)
	if loopPath := e.currentLoopIterationPath(nodeID, iterationCounters); loopPath != "" {
		parts = append(parts, loopPath)
	}
	if foreachPath := e.currentForeachIterationPath(nodeID, iterationCounters); foreachPath != "" {
		parts = append(parts, foreachPath)
	}
	return strings.Join(parts, ";")
}

// currentForeachIterationPath returns the stable, namespaced counters of all
// foreach bodies containing nodeID. Foreach does not retain a compiled Body
// set in the IR, so membership is reconstructed from the same graph property
// used for loops: a body node is reachable from a back-edge target and can
// reach a back-edge source without crossing another bounded-iteration edge.
// Endpoints are always included, which also supports hand-written/legacy IRs.
func (e *Engine) currentForeachIterationPath(nodeID string, iterationCounters map[string]int) string {
	if e == nil || e.workflow == nil || len(e.workflow.Foreaches) == 0 {
		return ""
	}

	forwardAdj := make(map[string][]string, len(e.workflow.Nodes))
	reverseAdj := make(map[string][]string, len(e.workflow.Nodes))
	for _, edge := range e.workflow.Edges {
		if edge == nil || edge.IsBoundedIteration() {
			continue
		}
		forwardAdj[edge.From] = append(forwardAdj[edge.From], edge.To)
		reverseAdj[edge.To] = append(reverseAdj[edge.To], edge.From)
	}

	reachable := func(seeds []string, adjacency map[string][]string) map[string]bool {
		visited := make(map[string]bool, len(seeds))
		queue := make([]string, 0, len(seeds))
		for _, seed := range seeds {
			if !visited[seed] {
				visited[seed] = true
				queue = append(queue, seed)
			}
		}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			for _, next := range adjacency[current] {
				if !visited[next] {
					visited[next] = true
					queue = append(queue, next)
				}
			}
		}
		return visited
	}

	names := make([]string, 0, len(e.workflow.Foreaches))
	for name, foreach := range e.workflow.Foreaches {
		if foreach == nil {
			continue
		}
		var sources, targets []string
		member := false
		for _, edge := range e.workflow.Edges {
			if edge == nil || edge.ForeachName != name {
				continue
			}
			sources = append(sources, edge.From)
			targets = append(targets, edge.To)
			if edge.From == nodeID || edge.To == nodeID {
				member = true
			}
		}
		if len(sources) == 0 {
			continue
		}
		if !member {
			forward := reachable(targets, forwardAdj)
			if forward[nodeID] {
				reverse := reachable(sources, reverseAdj)
				member = reverse[nodeID]
			}
		}
		if member {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}

	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		counterKey := foreachCounterKey(name)
		parts = append(parts, fmt.Sprintf("%s=%d", counterKey, iterationCounters[counterKey]))
	}
	return strings.Join(parts, ";")
}

func (e *Engine) outputCorrectionContext(ctx context.Context, rs *runState, nodeID string) (context.Context, context.CancelFunc, bool, error) {
	noop := func() {}
	if rs == nil || rs.budget == nil {
		return ctx, noop, false, nil
	}
	remaining, bounded := rs.budget.RemainingDuration()
	if !bounded {
		return ctx, noop, false, nil
	}
	if remaining <= 0 {
		if _, allowed := e.withinBudgetGrace(rs); allowed {
			remaining, _ = rs.budget.GracedRemainingDuration(budgetExitGraceRatio())
		}
	}
	if remaining <= 0 {
		return ctx, noop, false, outputCorrectionDurationError(nodeID)
	}
	deadline := time.Now().Add(remaining)
	budgetDeadline := true
	if parentDeadline, ok := ctx.Deadline(); ok && !deadline.Before(parentDeadline) {
		budgetDeadline = false
	}
	boundedCtx, cancel := context.WithDeadline(ctx, deadline)
	return boundedCtx, cancel, budgetDeadline, nil
}

func outputCorrectionDurationError(nodeID string) error {
	return &RuntimeError{
		Code:    ErrCodeBudgetExceeded,
		Message: fmt.Sprintf("output correction for node %q exhausted the run duration budget", nodeID),
		NodeID:  nodeID,
		Hint:    "increase max_duration or reduce output-correction work",
		Cause:   ErrBudgetExceeded,
	}
}

func correctionFingerprint(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		b = []byte(fmt.Sprintf("%#v", value))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) loadCorrectionEpisode(ctx context.Context, runID, ledgerKey string) (store.OutputCorrectionEpisode, bool, error) {
	if e.store == nil {
		return store.OutputCorrectionEpisode{}, false, nil
	}
	r, err := e.store.LoadRun(ctx, runID)
	if err != nil {
		return store.OutputCorrectionEpisode{}, false, err
	}
	if r == nil || r.OutputCorrections == nil {
		return store.OutputCorrectionEpisode{}, false, nil
	}
	ep, ok := r.OutputCorrections[ledgerKey]
	return ep, ok, nil
}

func (e *Engine) persistCorrectionEpisode(ctx context.Context, runID, ledgerKey string, episode store.OutputCorrectionEpisode) error {
	correctionStore := store.AsOutputCorrectionStore(e.store)
	if correctionStore == nil {
		return fmt.Errorf("store does not implement granular output-correction persistence")
	}
	return correctionStore.SetRunOutputCorrection(ctx, runID, ledgerKey, episode)
}

// ---------------------------------------------------------------------------
// Output utilities
// ---------------------------------------------------------------------------

// extractUsage reads conventional _tokens and _cost_usd keys from a node
// output. Returns zeros if absent.
func extractUsage(output map[string]any) (tokens int, costUSD float64) {
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
	// `cost.Annotate` writes `_cost_usd` only when a price was resolved, so
	// an absent (or zero) key means "no cost data", never "this call was
	// free" — the cost package says so in as many words and tells callers
	// not to record a $0 sample. A zero returned here is therefore "unknown";
	// SharedBudget.RecordUsage is what keeps it out of the enforced axis.
	return
}

// buildNodeFinishedData builds the data payload for a node_finished event,
// including usage metrics (_tokens, _cost_usd) and a snapshot of the output.
func buildNodeFinishedData(output map[string]any) map[string]any {
	if output == nil {
		return nil
	}
	data := map[string]any{
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

// SecretScrubber is optionally implemented by a NodeExecutor to scrub
// secret values from a node's output before the engine persists it to
// an OBSERVATIONAL sink (the node_finished event). It is Layer 0 of
// iterion's secrets protection.
//
// ScrubOutput MUST return a redacted deep copy and never mutate its
// input: the live output map feeds downstream nodes (`{{outputs.X}}` /
// `{{artifacts.X}}`) and the resume checkpoint, both of which must keep
// the real values. For that reason the engine applies scrubbing only on
// the event path here — NOT to persisted artifacts or the checkpoint,
// which are load-bearing for resume.
type SecretScrubber interface {
	ScrubOutput(map[string]any) map[string]any
}

// sanitizeOutputForEvent returns a copy of output scrubbed for the
// node_finished event stream. Two layers apply:
//
//   - Secret redaction (Layer 0): when the active executor implements
//     SecretScrubber, secret values are replaced with placeholders /
//     markers in a deep copy. The live output is untouched.
//   - The privacy_unfilter special-case: that tool's output carries the
//     restored text in the `text` field, which must not enter the event
//     stream (replaced with privacy.EventTextMarker).
//
// Returns the original map only when neither layer changes anything.
func (e *Engine) sanitizeOutputForEvent(node ir.Node, output map[string]any) map[string]any {
	if output == nil {
		return nil
	}
	out := output
	if _, hasBlob := out[delegate.SessionStateBlobKey]; hasBlob || out[delegate.SessionStateKey] != nil || out["_session_state_ref"] != nil {
		copied := make(map[string]any, len(out))
		for k, v := range out {
			copied[k] = v
		}
		delete(copied, delegate.SessionStateBlobKey)
		delete(copied, delegate.SessionStateKey)
		delete(copied, "_session_state_ref")
		out = copied
	}
	if scrubber, ok := e.executor.(SecretScrubber); ok {
		// ScrubOutput returns a redacted deep copy (never the original),
		// so the live output map is safe.
		out = scrubber.ScrubOutput(out)
	}
	if toolNode, ok := node.(*ir.ToolNode); ok && toolNode.Command == privacy.UnfilterToolName {
		if _, has := out["text"]; has {
			sanitized := make(map[string]any, len(out))
			for k, v := range out {
				sanitized[k] = v
			}
			sanitized["text"] = privacy.EventTextMarker
			out = sanitized
		}
	}
	return out
}
