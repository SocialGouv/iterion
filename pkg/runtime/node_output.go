package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
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
// output schema. Executor validation remains authoritative. Other mismatches
// warn by default and fail in strict mode.
func (e *Engine) validateNodeOutput(rs *runState, nodeID string, node ir.Node, output map[string]any) error {
	if e.executorValidatesOutput(node) {
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
		if !e.validateOutputs {
			fields := model.OutputSchemaMismatchFields(output, schema)
			e.logger.Warn("node_output_schema_mismatch: node %q schema %q fields %v", nodeID, schemaName, fields)
			return e.emit(rs.ctx, rs.runID, store.EventNodeOutputSchemaMismatch, nodeID, map[string]any{"schema": schemaName, "fields": fields})
		}
		code := ErrCodeSchemaValidation
		if _, tool := node.(*ir.ToolNode); tool {
			// A deterministic mismatch must not replay a tool's side effects on
			// cloud redelivery. LLM samples retain SCHEMA_VALIDATION semantics.
			code = ErrCodeToolFailedPermanent
		}
		return &RuntimeError{
			Code:    code,
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
	return e.correctAndValidateNodeOutputInScope(ctx, rs, nodeID, rs.runID, node, output)
}
func (e *Engine) correctAndValidateNodeOutputInScope(ctx context.Context, rs *runState, nodeID, scope string, node ir.Node, output map[string]any) (map[string]any, error) {
	validation := e.validateNodeOutput(rs, nodeID, node, output)
	if validation == nil {
		return output, nil
	}
	if accountedInvocation(rs, nodeID) && !rs.correctionInvocation.generic {
		return output, validation
	}
	return e.correctGenericOutput(ctx, rs, nodeID, scope, node, output, validation)
}

func (e *Engine) outputCorrectionSpendError(rs *runState, nodeID string, output map[string]any) error {
	if rs == nil || rs.budget == nil {
		return nil
	}
	spend := unbookedNodeSpend(rs, nodeID, output)
	if accountedInvocation(rs, nodeID) {
		spend = nodeSpend{}
	}
	status := rs.budget.Status()
	type axis struct {
		name        string
		used, limit float64
	}
	for _, candidate := range []axis{
		{name: "tokens", used: float64(status.Tokens + spend.tokens), limit: float64(status.MaxTokens)},
		{name: "cost_usd", used: status.CostUSD + spend.costUSD, limit: status.MaxCostUSD},
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
	return e.correctionIdentity(rs, nodeID, false)
}

func (e *Engine) correctionIdentity(rs *runState, nodeID string, legacy bool) (ledgerKey, invocationID string) {
	iterationPath := ""
	scope := ""
	if rs != nil {
		iterationPath = e.currentCorrectionIterationPath(nodeID, runStateIterationCounters(rs))
		if legacy {
			iterationPath = e.legacyCorrectionIterationPath(nodeID, runStateIterationCounters(rs))
		}
		scope = rs.correctionScope
		// A fan-out branch can terminate at a node outside the enclosing
		// loop's compiled body. Its branch ID repeats on the next fan-out
		// invocation, so node membership alone would make both executions
		// share one durable correction budget. Carry the trunk counters that
		// identify the enclosing parallel invocation even for such terminal
		// branch nodes. Root fan-outs retain their historical identity.
		if rs.branchLocal {
			if enclosingPath := branchCounterPath(rs.enclosingLoopCounters); enclosingPath != "root" {
				scope += "@" + enclosingPath
			}
		}
	}
	invocationID = fmt.Sprintf("node=%s;branch=%s;loops=%s", nodeID, scope, iterationPath)
	if scope == "" && iterationPath == "" && !strings.Contains(nodeID, ".") && !strings.HasPrefix(nodeID, "$") {
		return nodeID, invocationID
	}
	return "inv_" + correctionFingerprint(invocationID), invocationID
}

// currentCorrectionIterationPath includes the complete counter vector for a
// repeated node, including outer loops whose compiled body omits that node.
// Only workflows without loops or foreach retain bare node identities.
func (e *Engine) currentCorrectionIterationPath(nodeID string, counters map[string]int) string {
	if e == nil || e.workflow == nil || (len(e.workflow.Loops) == 0 && len(e.workflow.Foreaches) == 0) {
		return ""
	}
	parts := make([]string, 0, len(e.workflow.Loops)+len(e.workflow.Foreaches))
	for name := range e.workflow.Loops {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counters[name]))
	}
	for name := range e.workflow.Foreaches {
		key := foreachCounterKey(name)
		parts = append(parts, fmt.Sprintf("%s=%d", key, counters[key]))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func (e *Engine) legacyCorrectionIterationPath(nodeID string, iterationCounters map[string]int) string {
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
	episode.LastError = e.recordedFailureText(episode.LastError)
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

// nodeSpend is what a node output says it spent: tokens, cost, and how many
// of its calls went without the provider reporting their usage — for those
// the tokens and cost are a lower bound.
type nodeSpend struct {
	tokens          int
	costUSD         float64
	unreportedCalls int
}

// empty reports an output that spent nothing at all: no tokens, no cost, and
// no call whose usage went unreported — that one's bill is unknown, not zero.
func (s nodeSpend) empty() bool {
	return s.tokens == 0 && s.costUSD == 0 && s.unreportedCalls == 0
}

// onlyUnreported reports a spend that is nothing but unreported calls: no
// token or cost counter moved, so there is nothing to book, only a count to
// note.
func (s nodeSpend) onlyUnreported() bool {
	return s.tokens == 0 && s.costUSD == 0 && s.unreportedCalls > 0
}

// extractSpend reads a node output's spend: the `_tokens` / `_cost_usd` keys
// extractUsage reads, and the count of calls whose usage went unreported.
func extractSpend(output map[string]any) nodeSpend {
	tokens, costUSD := extractUsage(output)
	return nodeSpend{tokens: tokens, costUSD: costUSD, unreportedCalls: cost.UnreportedCalls(output)}
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
// The engine also runs a failure TEXT through it — the one the run document
// records, which the engine itself builds and no executor ever sees
// (recordedFailureText wraps the string in a one-key map, failure_record.go):
// one matcher for both sinks, never a second one beside it.
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

// loadInvocationCorrectionEpisode claims a legacy key for one new invocation.
// The owner marker lands first: an interrupted migration can finish for that
// owner, but a later outer iteration cannot reuse the same consumed allowance.
func (e *Engine) loadInvocationCorrectionEpisode(ctx context.Context, rs *runState, nodeID, key, invocation string) (store.OutputCorrectionEpisode, bool, error) {
	ep, found, err := e.loadCorrectionEpisode(ctx, rs.runID, key)
	if err != nil || found {
		return ep, found, err
	}
	legacyKey, _ := e.correctionIdentity(rs, nodeID, true)
	if legacyKey == key {
		return ep, false, nil
	}
	ep, found, err = e.loadCorrectionEpisode(ctx, rs.runID, legacyKey)
	if err != nil || !found {
		return ep, found, err
	}
	owner := "invocation_key_migrated:" + key
	if ep.RetiredReason != "" && ep.RetiredReason != owner {
		return store.OutputCorrectionEpisode{}, false, nil
	}
	if ep.RetiredReason == "" {
		now := time.Now().UTC()
		ep.RetiredAt = &now
		ep.RetiredReason = owner
		if err := e.persistCorrectionEpisode(ctx, rs.runID, legacyKey, ep); err != nil {
			return ep, false, err
		}
	}
	ep.EpisodeID = key
	ep.InvocationID = invocation
	ep.RetiredAt = nil
	ep.RetiredReason = ""
	if err := e.persistCorrectionEpisode(ctx, rs.runID, key, ep); err != nil {
		return ep, false, err
	}
	return ep, true, nil
}
