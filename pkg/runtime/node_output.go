package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
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

// defaultOutputCorrectionBudget is the machine default for the bounded
// invalid-output correction loop, used when no launch surface passes
// WithOutputCorrectionBudget.
const defaultOutputCorrectionBudget = 2

// resolveOutputCorrectionBudget reads the machine default:
// ITERION_OUTPUT_CORRECTION_BUDGET → defaultOutputCorrectionBudget. `0` (or
// off/no/false/none) disables correction entirely — the escape hatch a
// deployment needs to refuse the extra model calls outright, since a
// hardcoded constant bounding operator work with no override is a defect.
// Unparsable or negative values fall back to the DEFAULT rather than invent a
// policy: unlike the exit grace this is not a spend ceiling an operator
// tightens, it is a repair allowance, and reading a typo as "unbounded" is the
// one answer that is never right.
func resolveOutputCorrectionBudget() int {
	raw := strings.TrimSpace(os.Getenv("ITERION_OUTPUT_CORRECTION_BUDGET"))
	if raw == "" {
		return defaultOutputCorrectionBudget
	}
	switch strings.ToLower(raw) {
	case "off", "no", "false", "none":
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		correctionBudgetWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "iterion: ITERION_OUTPUT_CORRECTION_BUDGET=%q is not a non-negative integer — using the default %d\n", raw, defaultOutputCorrectionBudget)
		})
		return defaultOutputCorrectionBudget
	}
	return v
}

var correctionBudgetWarnOnce sync.Once

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

	// The ledger is keyed by NODE EXECUTION, not by node id: a bounded loop
	// re-executes the same node, and a raw node id would make every iteration
	// share one budget and inherit the previous one's terminal verdict. The key
	// is also Mongo-safe, which a raw node id is not — group expansion mints
	// dotted ids (`prefix.name`).
	//
	// The branch id is empty because correction is TRUNK-ONLY: fan-out branches
	// validate through validateNodeOutput directly (branch.go) and never reach
	// here. The key composition already carries a branch slot, so extending
	// correction into branches later needs no ledger migration.
	ledgerKey := e.executionScopedKey(nodeID, rs.loopCounters, "")
	if ledgerKey == "" {
		return output, validationErr
	}
	inputFingerprint := correctionPayloadFingerprint(output)
	violationFingerprint := correctionFingerprint(validationErr.Error())
	episode, found, loadErr := e.loadCorrectionEpisode(ctx, rs.runID, ledgerKey)
	if loadErr != nil {
		// Fail closed: without the ledger we cannot know how much budget this
		// episode already consumed, and guessing "none" is exactly the
		// unbounded loop the ledger exists to prevent.
		return output, fmt.Errorf("output correction ledger: %w (original validation: %v)", loadErr, validationErr)
	}
	// The budget this episode was actually stopped under. Captured BEFORE any
	// raise below, because that assignment is what tells "the operator raised
	// the cap" apart from "the same cap, simply re-entered" — and the raise
	// would otherwise destroy the evidence.
	persistedBudget := episode.Budget
	// The LEDGER KEY is the episode's identity — deliberately NOT the payload
	// fingerprint. An LLM node re-executed by a resume produces a DIFFERENT
	// invalid payload each time, so keying on the payload handed every resume
	// a fresh episode at Attempts=0: under --auto-resume / retrypolicy that is
	// precisely the unbounded correction loop this ledger exists to stop, and
	// it silently overwrote the record of what the earlier attempts had spent.
	// The fingerprint stays as EVIDENCE, and as the qualifier on the
	// `unchanged` verdict below.
	//
	// A `succeeded` episode is CLOSED, and only that resets the budget: the
	// previous execution ended on a valid payload, so this is new work rather
	// than a retry of a correction that never landed.
	if !found || episode.Status == correctionStatusSucceeded {
		now := time.Now().UTC()
		episode = store.OutputCorrectionEpisode{
			EpisodeID:                fmt.Sprintf("%s-%d", inputFingerprint[:min(16, len(inputFingerprint))], now.UnixNano()),
			NodeID:                   nodeID,
			Budget:                   e.outputCorrectionBudget,
			Status:                   correctionStatusActive,
			InputFingerprint:         inputFingerprint,
			LastOutputFingerprint:    inputFingerprint,
			LastViolationFingerprint: violationFingerprint,
			StartedAt:                now,
			UpdatedAt:                now,
		}
		found, persistedBudget = false, 0
	} else if episode.Budget < e.outputCorrectionBudget {
		// A launch may raise the budget, but never lower an already consumed
		// episode's bound. The persisted value remains the audit contract.
		episode.Budget = e.outputCorrectionBudget
	}
	samePayload := found && episode.InputFingerprint == inputFingerprint

	// Terminal statuses are enforced HERE, not by the loop bound below: an
	// `Attempts >= Budget` conjunct would be inert, since `for episode.Attempts
	// < episode.Budget` already short-circuits on it.
	switch episode.Status {
	case correctionStatusUnchanged:
		// Provably dead — for the payload it was proven on. The same payload
		// produced the same violation, so every further call is spend on a
		// known-dead path: terminal even with budget left, and even when the
		// operator raises it. A DIFFERENT payload was never tried, so it may
		// still be corrected, bounded by the attempts already spent.
		if samePayload {
			return output, validationErr
		}
	case correctionStatusExhausted:
		// Terminal at the budget it was stopped under — otherwise a resume
		// silently re-enters a closed episode and the persisted bound means
		// nothing. Only a genuine RAISE reopens it, and then only for the
		// difference: attempts already spent are never given back.
		if e.outputCorrectionBudget <= persistedBudget {
			return output, validationErr
		}
	}
	if episode.Budget <= 0 {
		return output, validationErr
	}
	if !samePayload {
		// Same node execution, new payload: carry the attempts already spent —
		// that accumulation IS the durable bound — and record what we are now
		// working on.
		episode.InputFingerprint = inputFingerprint
		episode.LastViolationFingerprint = violationFingerprint
		episode.Status = correctionStatusActive
	}

	// The primary node call runs under the run's remaining-duration deadline
	// (engine_exec.go wraps Execute in context.WithDeadline). Correction must
	// too, or a hung corrector runs UNBOUNDED after the bounded call it exists
	// to repair already returned — the one stall a max_duration cannot
	// terminate. Mirrors the primary path, grace included.
	correctCtx, cancelCorrect := e.correctionContext(ctx, rs)
	if cancelCorrect != nil {
		defer cancelCorrect()
	}
	if correctCtx.Err() != nil {
		// No wall clock left to spend: report the validation failure rather
		// than open a correction the run cannot afford to finish.
		return output, validationErr
	}

	current := output
	currentErr := validationErr
	for episode.Attempts < episode.Budget {
		if err := correctCtx.Err(); err != nil {
			// Out of time (or cancelled) between attempts: stop here and let
			// the run lifecycle classify it. The episode keeps the attempts it
			// really spent, so a resume does not re-buy them.
			return current, currentErr
		}
		episode.Status = correctionStatusActive
		episode.Attempts++
		episode.LastOutputFingerprint = correctionPayloadFingerprint(current)
		episode.LastViolationFingerprint = correctionFingerprint(currentErr.Error())
		episode.LastError = currentErr.Error()
		episode.UpdatedAt = time.Now().UTC()
		if err := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); err != nil {
			// Losing the durable ledger is safer than pretending an unbounded
			// correction is allowed: surface the original validation failure and
			// let the normal run lifecycle mark it resumable/failed.
			return current, fmt.Errorf("output correction ledger: %w (original validation: %v)", err, currentErr)
		}

		candidate, correctionErr := corrector.CorrectOutput(correctCtx, node, current, currentErr)
		// Carry the engine's own `_`-prefixed metadata across. The natural
		// corrector returns just the repaired business payload, and replacing
		// `output` wholesale would drop `_tokens`/`_cost_usd` — deleting the
		// node's entire contribution to the run budget and the daily spend cap
		// — along with `_backend`/`_model`/`_fallback_used`/`_served_by`, which
		// a downstream deterministic gate reads to fail closed on a degraded
		// input, and `_duration_ms`, which the report renders. A key the
		// corrector set itself always wins, so it can fold its OWN spend into
		// `_tokens`/`_cost_usd` (the only accounting channel it has).
		//
		// Merged from `current`, not from the node's original `output`: on the
		// second and later attempts `current` is the previous candidate, which
		// already carries the merge — so a corrector that reported its spend on
		// attempt 1 and stays silent on attempt 2 keeps that figure instead of
		// having it reset to the node's original usage.
		mergeEngineMetadata(candidate, current)
		if correctionErr != nil || candidate == nil {
			// NOT terminal. A 429, a usage window, a network blip or a
			// DeadlineExceeded is exactly what the failed_resumable + retry
			// machinery exists to recover from, and stamping `exhausted` here
			// closed the episode for good — with most of the budget unspent and
			// no raise able to reopen it on the retry pod, since the budget is
			// process-wide and identical there. The ATTEMPT we already charged
			// is the bound; a later re-entry that spends the rest lands on the
			// exhausted path below by itself. This also matches the ctx-expiry
			// stop above, which deliberately writes no terminal status either.
			episode.Status = correctionStatusActive
			if correctionErr != nil {
				episode.LastError = correctionErr.Error()
			} else {
				episode.LastError = "corrector returned a nil output"
			}
			episode.UpdatedAt = time.Now().UTC()
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
				return current, fmt.Errorf("output correction failed: %v; ledger: %w", episode.LastError, persistErr)
			}
			return current, currentErr
		}

		candidateErr := e.validateNodeOutput(nodeID, node, candidate)
		candidateFP := correctionPayloadFingerprint(candidate)
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
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
				return candidate, fmt.Errorf("output correction succeeded but ledger could not be persisted: %w", persistErr)
			}
			return candidate, nil
		}

		// A corrector that returns the exact same invalid payload is a
		// no-progress loop. Stop immediately, even when budget remains.
		if candidateFP == correctionPayloadFingerprint(current) && candidateViolationFP == correctionFingerprint(currentErr.Error()) {
			episode.Status = correctionStatusUnchanged
			episode.LastOutputFingerprint = candidateFP
			episode.LastViolationFingerprint = candidateViolationFP
			episode.LastError = candidateErr.Error()
			episode.UpdatedAt = time.Now().UTC()
			if persistErr := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); persistErr != nil {
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
	if err := e.persistCorrectionEpisode(ctx, rs.runID, ledgerKey, episode); err != nil {
		return current, fmt.Errorf("output correction budget exhausted; ledger: %w", err)
	}
	return current, currentErr
}

// failValidationAfterCorrection classifies a validation failure that survived
// the correction loop.
//
// That loop is the first thing in the post-exec pipeline that can BLOCK — it
// calls out to the executor and waits — so the run can be torn down (operator
// cancel, runner drain, wall-clock deadline) while the engine sits in it.
// Validation used to be instantaneous, which is why the post-exec path never
// needed the cause-aware routing the exec path has. When the RUN context is
// done, that teardown is what happened: route it the same way, because a
// generic fail stringifies the error and loses the sentinel — a drain would
// surface as a spurious "run failed" instead of a silent auto-resume, and an
// operator cancel would land failed_resumable and get redelivered-resumed.
func (e *Engine) failValidationAfterCorrection(ctx context.Context, rs *runState, nodeID string, validationErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return e.handleContextDoneWithCheckpoint(rs, nodeID, ctxErr)
	}
	return e.failRunErrWithCheckpoint(rs, nodeID, validationErr)
}

// correctionContext bounds the correction loop by the run's remaining
// wall-clock duration, exactly as execLoop bounds the primary node call. An
// unbounded run (no max_duration) yields ctx unchanged and a nil cancel.
func (e *Engine) correctionContext(ctx context.Context, rs *runState) (context.Context, context.CancelFunc) {
	if rs == nil || rs.budget == nil {
		return ctx, nil
	}
	rem, bounded := rs.budget.RemainingDuration()
	if !bounded {
		return ctx, nil
	}
	if rem <= 0 {
		// Inside the duration grace: bound by the GRACED ceiling rather than
		// run deadline-less, the same choice execLoop makes.
		rem, _ = rs.budget.GracedRemainingDuration(budgetExitGraceRatio())
	}
	if rem <= 0 {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return cancelled, cancel
	}
	return context.WithDeadline(ctx, time.Now().Add(rem))
}

// correctionPayloadFingerprint hashes only the SCHEMA/business payload: every
// `_`-prefixed key is engine or provenance metadata and is excluded.
//
// This is what makes the episode identity stable. The engine stamps
// `_duration_ms` on every node output before validation runs, so hashing the
// whole map gave a different InputFingerprint on every execution — a resume or
// a loop re-entry always minted a FRESH episode at Attempts=0 and the
// persisted bound, terminal status included, was never once consulted. It also
// keeps the no-progress guard honest in the other direction: a corrector that
// folds its own spend into `_tokens` must not thereby look like progress.
func correctionPayloadFingerprint(output map[string]any) string {
	payload := make(map[string]any, len(output))
	for k, v := range output {
		if strings.HasPrefix(k, "_") {
			continue
		}
		payload[k] = v
	}
	return correctionFingerprint(payload)
}

// mergeEngineMetadata copies the `_`-prefixed keys of src onto dst, leaving any
// key dst already set untouched. Both may be nil.
func mergeEngineMetadata(dst, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	for k, v := range src {
		if !strings.HasPrefix(k, "_") {
			continue
		}
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
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

// loadCorrectionEpisode reads the persisted episode for key. It FAILS CLOSED:
// a store error is returned, never folded into "no episode". Reading a
// transient LoadRun failure as "none" would manufacture a fresh episode at
// Attempts=0 and — once the store recovers for the very next write — persist
// that over an exhausted/unchanged one, silently resetting the durable bound
// this ledger exists to hold.
func (e *Engine) loadCorrectionEpisode(ctx context.Context, runID, key string) (store.OutputCorrectionEpisode, bool, error) {
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
	ep, ok := r.OutputCorrections[key]
	return ep, ok, nil
}

// correctionLedgerRetries / correctionLedgerBackoff bound the CAS retry on the
// whole-document ledger write. Every partial run write bumps the CAS version
// (FilesystemRunStore.writeRun; versionRunUpdate on Mongo), so a concurrent
// granular write — an operator cancel, the orphan sweeper — makes this write
// CONFLICT rather than silently revert it. The cost is therefore contention,
// not data loss: without a pause between attempts, four retries fired
// microseconds apart all lose to the same competing writer and the loop
// reports a hard ledger failure in place of the validation error it was called
// to repair.
const (
	correctionLedgerRetries = 4
	correctionLedgerBackoff = 5 * time.Millisecond
)

func (e *Engine) persistCorrectionEpisode(ctx context.Context, runID, key string, episode store.OutputCorrectionEpisode) error {
	if e.store == nil {
		return nil
	}
	var lastErr error = store.ErrRunConflict
	for attempt := 0; attempt < correctionLedgerRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(correctionLedgerBackoff << (attempt - 1)):
			}
		}
		r, err := e.store.LoadRun(ctx, runID)
		if err != nil {
			return err
		}
		if r.OutputCorrections == nil {
			r.OutputCorrections = make(map[string]store.OutputCorrectionEpisode)
		}
		r.OutputCorrections[key] = episode
		err = e.store.SaveRun(ctx, r)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrRunConflict) {
			return err
		}
		lastErr = err
	}
	return lastErr
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
