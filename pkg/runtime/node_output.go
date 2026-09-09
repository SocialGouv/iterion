package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	correctionStatusActive    = "active"
	correctionStatusSucceeded = "succeeded"
	correctionStatusExhausted = "exhausted"
	correctionStatusUnchanged = "unchanged"
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

	corrector, ok := e.executor.(OutputCorrector)
	if !ok || e.outputCorrectionBudget <= 0 || e.store == nil {
		return output, validationErr
	}

	inputFingerprint := correctionFingerprint(output)
	violationFingerprint := correctionFingerprint(validationErr.Error())
	episode, found := e.loadCorrectionEpisode(ctx, rs.runID, nodeID)
	// InputFingerprint identifies the episode. The violation can legitimately
	// change while a corrector improves a payload, so comparing it here would
	// accidentally reset an exhausted episode on resume.
	if !found || episode.InputFingerprint != inputFingerprint {
		now := time.Now().UTC()
		episode = store.OutputCorrectionEpisode{
			EpisodeID:                fmt.Sprintf("%s-%d", inputFingerprint[:minInt(16, len(inputFingerprint))], now.UnixNano()),
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
		// episode's bound. The persisted value remains the audit contract.
		episode.Budget = e.outputCorrectionBudget
	}

	if (episode.Status == correctionStatusExhausted || episode.Status == correctionStatusUnchanged) && episode.Attempts >= episode.Budget {
		return output, validationErr
	}
	if episode.Budget <= 0 {
		return output, validationErr
	}

	current := output
	currentErr := validationErr
	for episode.Attempts < episode.Budget {
		episode.Status = correctionStatusActive
		episode.Attempts++
		episode.LastOutputFingerprint = correctionFingerprint(current)
		episode.LastViolationFingerprint = correctionFingerprint(currentErr.Error())
		episode.LastError = currentErr.Error()
		episode.UpdatedAt = time.Now().UTC()
		if err := e.persistCorrectionEpisode(ctx, rs.runID, nodeID, episode); err != nil {
			// Losing the durable ledger is safer than pretending an unbounded
			// correction is allowed: surface the original validation failure and
			// let the normal run lifecycle mark it resumable/failed.
			return current, fmt.Errorf("output correction ledger: %w (original validation: %v)", err, currentErr)
		}

		candidate, correctionErr := corrector.CorrectOutput(ctx, node, current, currentErr)
		if correctionErr != nil || candidate == nil {
			episode.Status = correctionStatusExhausted
			if correctionErr != nil {
				episode.LastError = correctionErr.Error()
			} else {
				episode.LastError = "corrector returned a nil output"
			}
			episode.UpdatedAt = time.Now().UTC()
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, nodeID, episode); persistErr != nil {
				return current, fmt.Errorf("output correction failed: %v; ledger: %w", episode.LastError, persistErr)
			}
			return current, currentErr
		}

		candidateErr := e.validateNodeOutput(nodeID, node, candidate)
		candidateFP := correctionFingerprint(candidate)
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
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, nodeID, episode); persistErr != nil {
				return candidate, fmt.Errorf("output correction succeeded but ledger could not be persisted: %w", persistErr)
			}
			return candidate, nil
		}

		// A corrector that returns the exact same invalid payload is a
		// no-progress loop. Stop immediately, even when budget remains.
		if candidateFP == correctionFingerprint(current) && candidateViolationFP == correctionFingerprint(currentErr.Error()) {
			episode.Status = correctionStatusUnchanged
			episode.LastOutputFingerprint = candidateFP
			episode.LastViolationFingerprint = candidateViolationFP
			episode.LastError = candidateErr.Error()
			episode.UpdatedAt = time.Now().UTC()
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, nodeID, episode); persistErr != nil {
				return candidate, fmt.Errorf("unchanged output correction; ledger: %w", persistErr)
			}
			return candidate, candidateErr
		}

		current = candidate
		currentErr = candidateErr
		episode.LastOutputFingerprint = candidateFP
		episode.LastViolationFingerprint = candidateViolationFP
		episode.LastError = candidateErr.Error()
	}

	episode.Status = correctionStatusExhausted
	episode.UpdatedAt = time.Now().UTC()
	if err := e.persistCorrectionEpisode(ctx, rs.runID, nodeID, episode); err != nil {
		return current, fmt.Errorf("output correction budget exhausted; ledger: %w", err)
	}
	return current, currentErr
}

func correctionFingerprint(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		b = []byte(fmt.Sprintf("%#v", value))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (e *Engine) loadCorrectionEpisode(ctx context.Context, runID, nodeID string) (store.OutputCorrectionEpisode, bool) {
	if e.store == nil {
		return store.OutputCorrectionEpisode{}, false
	}
	r, err := e.store.LoadRun(ctx, runID)
	if err != nil || r == nil || r.OutputCorrections == nil {
		return store.OutputCorrectionEpisode{}, false
	}
	ep, ok := r.OutputCorrections[nodeID]
	return ep, ok
}

func (e *Engine) persistCorrectionEpisode(ctx context.Context, runID, nodeID string, episode store.OutputCorrectionEpisode) error {
	if e.store == nil {
		return nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		r, err := e.store.LoadRun(ctx, runID)
		if err != nil {
			return err
		}
		if r.OutputCorrections == nil {
			r.OutputCorrections = make(map[string]store.OutputCorrectionEpisode)
		}
		r.OutputCorrections[nodeID] = episode
		if err := e.store.SaveRun(ctx, r); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrRunConflict) {
			return err
		}
	}
	return store.ErrRunConflict
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
