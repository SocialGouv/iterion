package runview

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// DiagnosticVersion is the wire version of DiagnosticProjection. The
// projection is deliberately additive: consumers can ignore fields they do
// not know while the persisted run and event formats remain unchanged.
const DiagnosticVersion = 1

// DiagnosticOutcome is a conclusion for an operator or a watcher. It is not
// a persisted RunStatus and must never be used as a replacement for one.
type DiagnosticOutcome string

const (
	DiagnosticActive      DiagnosticOutcome = "active"
	DiagnosticFinished    DiagnosticOutcome = "finished"
	DiagnosticBlocked     DiagnosticOutcome = "blocked"
	DiagnosticInterrupted DiagnosticOutcome = "interrupted"
	DiagnosticFailed      DiagnosticOutcome = "failed"
)

// DiagnosticAction is the next safe action suggested by the persisted
// lifecycle state. It intentionally stays coarse: interpreting arbitrary
// workflow output as an engine policy would create a second, unreliable
// business-rule validator.
type DiagnosticAction string

const (
	DiagnosticActionNone          DiagnosticAction = "none"
	DiagnosticActionResume        DiagnosticAction = "resume"
	DiagnosticActionWait          DiagnosticAction = "wait"
	DiagnosticActionAnswer        DiagnosticAction = "answer_or_resume"
	DiagnosticActionInspect       DiagnosticAction = "inspect"
	DiagnosticActionFixThenResume DiagnosticAction = "fix_then_resume"
)

// DiagnosticEvidence is a bounded, allow-listed projection of an event.
// Arbitrary output payloads are deliberately excluded; only fields whose
// purpose is operational diagnosis are copied.
type DiagnosticEvidence struct {
	Seq     int64             `json:"seq"`
	Type    store.EventType   `json:"type"`
	NodeID  string            `json:"node_id,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// DiagnosticProjection is the common read model exposed by runview and the
// HTTP API. It combines the typed run outcome with the latest bounded event
// evidence so CLI, Studio, Copi and watchers can consume the same contract.
type DiagnosticProjection struct {
	Version     int                  `json:"version"`
	RunID       string               `json:"run_id"`
	ParentRunID string               `json:"parent_run_id,omitempty"`
	Status      store.RunStatus      `json:"status"`
	Outcome     DiagnosticOutcome    `json:"outcome"`
	Recoverable bool                 `json:"recoverable"`
	FailureCode store.FailureCode    `json:"failure_code,omitempty"`
	NodeID      string               `json:"node_id,omitempty"`
	Message     string               `json:"message,omitempty"`
	NextAction  DiagnosticAction     `json:"next_action"`
	Evidence    []DiagnosticEvidence `json:"evidence,omitempty"`
}

const (
	diagnosticEvidenceLimit = 12
	diagnosticValueLimit    = 2048
)

// BuildDiagnostic loads a run and projects its current diagnostic state.
// ScanEvents keeps memory bounded even for long runs; only the latest small
// set of operational events is retained.
func BuildDiagnostic(ctx context.Context, s store.RunStore, runID string) (*DiagnosticProjection, error) {
	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	d := newDiagnosticProjection(run)
	if err := s.ScanEvents(ctx, runID, d.observeEvent); err != nil {
		return nil, err
	}
	d.finalize(run)
	return d, nil
}

func newDiagnosticProjection(run *store.Run) *DiagnosticProjection {
	d := &DiagnosticProjection{
		Version:    DiagnosticVersion,
		NextAction: DiagnosticActionNone,
	}
	if run != nil {
		d.RunID = run.ID
		d.ParentRunID = run.ParentRunID
		d.Status = run.Status
		d.FailureCode = run.FailureCode
		d.Message = run.Error
		d.setStatusOutcome(run.Status)
		d.recomputeAction(run)
	}
	return d
}

// observeEvent keeps only the current execution episode. A resume starts a
// new episode, so a watcher does not mistake an old failure for a new one.
func (d *DiagnosticProjection) observeEvent(evt *store.Event) bool {
	if evt == nil {
		return true
	}
	switch evt.Type {
	case store.EventRunStarted, store.EventRunResumed, store.EventRunAutoResumed:
		d.Evidence = nil
		d.NodeID = ""
		d.Message = ""
		d.FailureCode = ""
		d.Outcome = DiagnosticActive
		d.Recoverable = false
		d.NextAction = DiagnosticActionNone
		return true
	case store.EventRunFinished:
		d.Outcome = DiagnosticFinished
		d.Recoverable = false
		d.NextAction = DiagnosticActionNone
		return true
	case store.EventRunInterrupted:
		d.Outcome = DiagnosticInterrupted
		d.Recoverable = true
		d.NextAction = DiagnosticActionResume
	case store.EventRunPaused:
		d.Outcome = DiagnosticBlocked
		d.Recoverable = true
		d.NextAction = DiagnosticActionAnswer
	case store.EventHumanInputRequested:
		// Async questions do not pause the run and therefore must not
		// trigger a watcher resume/answer loop.
		if store.IsAsyncHumanInput(*evt) {
			return true
		}
		d.Outcome = DiagnosticBlocked
		d.Recoverable = true
		d.NextAction = DiagnosticActionAnswer
	case store.EventRunCancelled:
		d.Outcome = DiagnosticFailed
		d.Recoverable = false
		d.NextAction = DiagnosticActionInspect
	case store.EventRunFailed:
		resumable, _ := evt.Data["resumable"].(bool)
		interrupted, _ := evt.Data["interrupted"].(bool)
		if interrupted || diagnosticCode(evt.Data) == string(store.FailureInterrupted) {
			d.Outcome = DiagnosticInterrupted
			d.Recoverable = true
			d.NextAction = DiagnosticActionResume
		} else if resumable {
			d.Outcome = DiagnosticBlocked
			d.Recoverable = true
			d.NextAction = DiagnosticActionResume
		} else {
			d.Outcome = DiagnosticFailed
			d.Recoverable = false
			d.NextAction = DiagnosticActionInspect
		}
	case store.EventRunRetryScheduled:
		d.Outcome = DiagnosticBlocked
		d.Recoverable = true
		d.NextAction = DiagnosticActionWait
	case store.EventRunRetrySkipped:
		d.Outcome = DiagnosticFailed
		d.Recoverable = false
		d.NextAction = DiagnosticActionInspect
	case store.EventNodeRecovery:
		// Keep the run active while a bounded node retry is in progress.
		if d.Outcome == "" || d.Outcome == DiagnosticFinished {
			d.Outcome = DiagnosticActive
		}
	case store.EventRunHealth:
		// Health evidence is useful to watchers, but its kind is not an
		// engine failure classification. Do not change the outcome here.
	case store.EventNodeFinished:
		// Node output is an observational proof only. In particular, the
		// declared `output.detail` field must be visible to diagnostics,
		// while the rest of the arbitrary output remains private.
	default:
		return true
	}

	details := projectDiagnosticDetails(evt.Data)
	if len(details) == 0 && evt.Type != store.EventRunFailed && evt.Type != store.EventRunInterrupted {
		return true
	}
	d.Evidence = append(d.Evidence, DiagnosticEvidence{
		Seq:     evt.Seq,
		Type:    evt.Type,
		NodeID:  evt.NodeID,
		Details: details,
	})
	if len(d.Evidence) > diagnosticEvidenceLimit {
		d.Evidence = d.Evidence[len(d.Evidence)-diagnosticEvidenceLimit:]
	}
	if evt.NodeID != "" {
		d.NodeID = evt.NodeID
	}
	if code := diagnosticCode(evt.Data); code != "" {
		d.FailureCode = store.FailureCode(code)
	}
	if msg, ok := diagnosticString(evt.Data, "error"); ok {
		d.Message = msg
	}
	return true
}

func (d *DiagnosticProjection) finalize(run *store.Run) {
	if run == nil {
		return
	}
	d.RunID = run.ID
	d.ParentRunID = run.ParentRunID
	d.Status = run.Status
	if run.FailureCode != "" {
		d.FailureCode = run.FailureCode
	}
	if run.Error != "" {
		d.Message = run.Error
	}
	if d.NodeID == "" && len(d.Evidence) > 0 {
		d.NodeID = d.Evidence[len(d.Evidence)-1].NodeID
	}
	d.setStatusOutcome(run.Status)
	d.recomputeAction(run)
}

// refreshRun updates run-level fields while preserving the current event
// episode. SnapshotBuilder calls this when a live run document changes.
func (d *DiagnosticProjection) refreshRun(run *store.Run) {
	if run == nil {
		return
	}
	d.RunID = run.ID
	d.ParentRunID = run.ParentRunID
	d.Status = run.Status
	if run.FailureCode != "" {
		d.FailureCode = run.FailureCode
	}
	if run.Error != "" {
		d.Message = run.Error
	}
	d.setStatusOutcome(run.Status)
	d.recomputeAction(run)
}

func (d *DiagnosticProjection) setStatusOutcome(status store.RunStatus) {
	switch {
	case status.IsFinalSuccess():
		d.Outcome, d.Recoverable = DiagnosticFinished, false
	case status == store.RunStatusFailedResumable:
		if d.Outcome != DiagnosticInterrupted {
			d.Outcome = DiagnosticBlocked
		}
		d.Recoverable = true
	case status.IsPaused():
		d.Outcome, d.Recoverable = DiagnosticBlocked, true
	case status.IsFinalFailure():
		d.Outcome, d.Recoverable = DiagnosticFailed, false
	case status == store.RunStatusCancelled:
		d.Outcome, d.Recoverable = DiagnosticFailed, false
	case status == store.RunStatusRunning:
		d.Outcome, d.Recoverable = DiagnosticActive, false
	case status.IsQueued():
		d.Outcome, d.Recoverable = DiagnosticActive, false
	default:
		d.Outcome, d.Recoverable = DiagnosticFailed, false
	}
}

func (d *DiagnosticProjection) recomputeAction(run *store.Run) {
	switch {
	case d.Status.IsFinalSuccess():
		d.NextAction = DiagnosticActionNone
	case d.Status == store.RunStatusRunning:
		d.NextAction = DiagnosticActionNone
	case d.Status.IsQueued():
		d.NextAction = DiagnosticActionNone
	case d.Status.IsPaused():
		d.NextAction = DiagnosticActionAnswer
	case d.Status == store.RunStatusFailedResumable:
		if run != nil && run.RetryState != nil && run.RetryState.RetryAfter != nil {
			d.NextAction = DiagnosticActionWait
		} else {
			d.NextAction = DiagnosticActionResume
		}
	case d.Status.IsFinalFailure():
		d.NextAction = DiagnosticActionInspect
	case d.Status == store.RunStatusCancelled:
		d.NextAction = DiagnosticActionInspect
	default:
		d.NextAction = DiagnosticActionInspect
	}
}

func projectDiagnosticDetails(data map[string]any) map[string]string {
	if len(data) == 0 {
		return nil
	}
	keys := []string{
		"code", "error", "phase", "reason", "resumable", "interrupted",
		"retry_after", "attempt", "max_attempts", "delay_ms", "reset_source",
		"kind", "axis", "budget_pct", "detail",
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := diagnosticScalar(data[key]); ok {
			out[key] = value
		}
	}
	// node_finished wraps the producer output under `output`. Preserve only
	// its declared scalar detail as evidence; never flatten the arbitrary
	// output map into the operator-facing projection.
	if _, exists := out["detail"]; !exists {
		if output, ok := data["output"].(map[string]any); ok {
			if detail, ok := diagnosticScalar(output["detail"]); ok {
				out["detail"] = detail
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func diagnosticCode(data map[string]any) string {
	if code, ok := diagnosticString(data, "code"); ok {
		return code
	}
	return ""
}

func diagnosticString(data map[string]any, key string) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	value, ok := data[key]
	if !ok {
		return "", false
	}
	return diagnosticScalar(value)
}

func diagnosticScalar(value any) (string, bool) {
	var out string
	switch v := value.(type) {
	case string:
		out = v
	case bool:
		out = strconv.FormatBool(v)
	case int:
		out = strconv.Itoa(v)
	case int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		out = fmt.Sprint(v)
	case float32, float64:
		out = fmt.Sprint(v)
	case json.Number:
		out = v.String()
	default:
		return "", false
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", false
	}
	if len(out) > diagnosticValueLimit {
		out = out[:diagnosticValueLimit] + "…"
	}
	return out, true
}
