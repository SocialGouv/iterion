package runner

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

type preEngineVerdict struct {
	Phase         string
	Code          store.FailureCode
	Hint          string
	Disposition   store.PreEngineDisposition
	FinalStatus   store.RunStatus
	Details       map[string]any
	RunnerVersion string
	Timeout       time.Duration
}

// refuseBeforeEngine returns the identical error. Delivery classification,
// bundle cleanup and retry ownership remain with processOne. Only callers
// that have not entered Engine.Run/Resume use this adapter.
func (r *Runner) refuseBeforeEngine(ctx context.Context, msg *queue.RunMessage, phase string, cause error) error {
	v := preEngineVerdict{Phase: phase, Code: refusalCode(cause), Disposition: store.PreEngineRetryPending}
	if v.Code == "" {
		v.Code = store.FailureExecutionFailed
	}
	var runtimeErr *runtime.RuntimeError
	if errors.As(cause, &runtimeErr) {
		v.Hint = runtimeErr.Hint
	}
	if !isNakAction(classifyExecResult(cause, msg.RunID).action) {
		v.Disposition = store.PreEngineFinalVerdict
		v.FinalStatus = store.RunStatusFailedResumable
	}
	r.recordPreEngineRefusal(ctx, msg, cause, v)
	return cause
}

// recordPreEngineRefusal is the single writer for pre-engine refusals. An
// absent optional capability is explicit degradation, never permission for
// a status-only fallback. Events follow only successful writes, and describe
// the returned status; document and AppendEvent are not an atomic transaction.
func (r *Runner) recordPreEngineRefusal(ctx context.Context, msg *queue.RunMessage, cause error, v preEngineVerdict) {
	if r.cfg.Store == nil || msg == nil || cause == nil {
		return
	}
	recorder, ok := r.cfg.Store.(store.PreEngineRefusalRecorder)
	if !ok {
		r.cfg.Logger.Warn("runner: run %s: pre-engine refusal %s not recorded: store lacks PreEngineRefusalRecorder", msg.RunID, v.Phase)
		return
	}
	var publishedAt time.Time
	if msg.PublishedAtRFC != "" {
		var err error
		publishedAt, err = time.Parse(time.RFC3339Nano, msg.PublishedAtRFC)
		if err != nil || publishedAt.IsZero() {
			r.cfg.Logger.Warn("runner: run %s: pre-engine refusal %s not recorded: invalid published_at", msg.RunID, v.Phase)
			return
		}
	}
	message := runtime.OperatorMessage(cause)
	if v.Hint != "" && !strings.Contains(message, v.Hint) {
		message += "; " + v.Hint
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = parkStoreOpTimeout
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	idCtx := store.WithIdentity(wctx, msg.TenantID, msg.OwnerID)
	q := store.PreEngineRefusalRequest{Phase: v.Phase, Code: v.Code, Message: message, Hint: v.Hint, PublishedAt: publishedAt, Disposition: v.Disposition, FinalStatus: v.FinalStatus,
		LeaveQueued: v.Disposition == store.PreEngineFinalVerdict && r.releasesRefusedResumes(msg)}
	result, err := recorder.RecordPreEngineRefusal(idCtx, msg.RunID, q)
	if err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not record pre-engine refusal %s: %v", msg.RunID, v.Phase, err)
		return
	}
	if !result.Changed {
		if !q.LeaveQueued {
			r.cfg.Logger.Warn("runner: run %s: pre-engine refusal %s declined: state or publication changed", msg.RunID, v.Phase)
		}
		return
	}
	data := make(map[string]any, len(v.Details)+8)
	for k, value := range v.Details {
		data[k] = value
	}
	version := v.RunnerVersion
	if version == "" {
		version = appinfo.Version
	}
	// Caller-specific details cannot overwrite the canonical verdict.
	data["code"], data["error"], data["hint"] = string(v.Code), message, v.Hint
	data["phase"], data["published_at"] = v.Phase, msg.PublishedAtRFC
	data["runner_version"], data["runner_commit"], data["status"] = version, appinfo.Commit, string(result.Status)
	typ := store.EventRunFailed
	if result.Status.IsQueued() {
		typ = store.EventRunPreEngineRefused
	}
	if _, err := r.cfg.Store.AppendEvent(idCtx, msg.RunID, store.Event{Type: typ, Data: data}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit %s for pre-engine refusal %s: %v", msg.RunID, typ, v.Phase, err)
	}
}
