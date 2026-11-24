package runtime

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// The recorded failure text — Layer 0 on the run document
// ---------------------------------------------------------------------------

// recordedFailureKey is the key recordedFailureText hands the executor's
// SecretScrubber. The run document carries a failure STRING while the only
// secret matcher a run owns speaks output maps (Layer 0, docs/secrets.md):
// the string rides through that one seam rather than beside a second matcher
// of its own, which would drift from it — the reason pkg/errtrack is extended
// and never duplicated.
const recordedFailureKey = "error"

// unscrubbableFailureText replaces a message the run's scrubber did not give
// back. It names the defect instead of staying empty: an empty error on a
// failed run reads to an operator as "no reason was recorded".
const unscrubbableFailureText = "(failure text withheld: the run's secret scrubber did not return it — see the engine log)"

// recordedFailureText is what a failure writes on the run DOCUMENT: the
// message with every known secret value replaced by its placeholder.
//
// A tool node's error embeds the command's streams ("shell command failed:
// …\nstdout: …\nstderr: …", pkg/backend/model/executor_tool.go), so a secret
// the tool printed travels inside the engine's own failure text. The event
// path is scrubbed at emitBranch, which is why events.jsonl, the "Run failed"
// console line (hence run.log) and a supervisor's prompt carry the
// placeholder while the run document still held the value (iterion#2037).
//
// The document is an OBSERVATIONAL sink, like the event path: the studio and
// the run views display it, `iterion inspect --json` prints it, the merge
// gate's synthetic status quotes it, pkg/alert fingerprints it. Nothing reads
// it back to decide — a usage-window retry reads the LIVE error chain (typed
// error, then runtime code, then that error's flattened text:
// usageWindowEvidence in pkg/runner), never the record. The checkpoint, which
// IS load-bearing for a resume, is not touched here.
func (e *Engine) recordedFailureText(msg string) string {
	if msg == "" {
		return msg
	}
	scrubber, ok := e.executor.(SecretScrubber)
	if !ok {
		return msg
	}
	text, ok := scrubber.ScrubOutput(map[string]any{recordedFailureKey: msg})[recordedFailureKey].(string)
	if !ok {
		// Persisting the raw message here would publish exactly what this
		// call exists to withhold, so the record keeps the marker — and the
		// log names the executor, because the seam is what needs fixing.
		if e.logger != nil {
			e.logger.Error("runtime: executor %T did not return the failure text from ScrubOutput — recording a withheld marker rather than a message the run's redactor never passed; ScrubOutput must return every key it is given", e.executor)
		}
		return unscrubbableFailureText
	}
	return text
}

// recordFailureResumable, recordFailureTerminal, recordFailureStatus and
// recordFailureStatusIf are THE four ways a failure text reaches the run
// document. Every path writes through one of them — a node's execution
// failure, a deliberate `fail:` node, an infrastructure interruption, a
// timeout, an operator cancel, a resume's sandbox death, the pre-resume
// budget preflight, a child-resource restore, the setup phase — so the scrub
// is one chokepoint instead of a guard repeated at nine call sites, and
// TestEngineSweep_EveryRecordedFailureTextIsScrubbed keeps the tenth from
// writing around it.
func (e *Engine) recordFailureResumable(ctx context.Context, runID string, cp *store.Checkpoint, msg string, code store.FailureCode) error {
	return e.store.FailRunResumable(ctx, runID, cp, e.recordedFailureText(msg), code)
}

func (e *Engine) recordFailureTerminal(ctx context.Context, runID string, cp *store.Checkpoint, msg string, code store.FailureCode) error {
	return e.store.FailRunTerminal(ctx, runID, cp, e.recordedFailureText(msg), code)
}

func (e *Engine) recordFailureStatus(ctx context.Context, runID string, status store.RunStatus, msg string, code store.FailureCode) error {
	return e.store.UpdateRunStatusCoded(ctx, runID, status, e.recordedFailureText(msg), code)
}

func (e *Engine) recordFailureStatusIf(ctx context.Context, runID string, status store.RunStatus, msg string, code store.FailureCode, expectedFrom []store.RunStatus) (bool, error) {
	return e.store.UpdateRunStatusIfCoded(ctx, runID, status, e.recordedFailureText(msg), code, expectedFrom)
}
