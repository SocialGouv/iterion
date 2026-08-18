package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The operator's subscription cap, runner side.
//
// Two jobs live here. The first is PUBLISHING what a run measures: every
// pod sees only its own session, so without a shared record each of them
// rediscovers the ceiling by spending against it. The second is the
// PRE-FLIGHT: a claimed run asks what the fleet already knows before it
// clones a repo or starts a container, and parks for free when the answer
// is "no headroom".
//
// Parking reuses the provider-refusal path wholesale (usage_retry.go): the
// run is marked failed_resumable with the cap as its error, a durable retry
// is armed for the instant the window reopens, and the delivery is acked.
// The operator's ceiling therefore inherits, for free, the one property
// that matters — a capped run is not lost, it is waiting.

// usageCapKey identifies the credential whose windows a run draws on.
//
// A tenant that brought its own subscription must not be blocked by what
// another tenant spent, and runs that fall back to the deployment's own
// credential must be pooled together — they really are one meter. The run's
// resolved credentials answer both: a bundle carrying an Anthropic key or
// OAuth dir is the tenant's own, anything else is the platform's.
func usageCapKey(ctx context.Context, msg *queue.RunMessage) string {
	scope := usagecap.ScopePlatform
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		if creds.APIKey(secrets.ProviderAnthropic) != "" || creds.OAuthDir(delegate.BackendClaudeCode) != "" {
			scope = usagecap.TenantScope(msg.TenantID)
		}
	}
	return usagecap.Key(delegate.BackendClaudeCode, scope)
}

// usageGuardFor builds the guard for one run: the machine-wide policy, with
// every reading published to the shared store under the run's credential
// key. Returns nil when no cap is configured, which keeps the whole feature
// out of the way of a deployment that never asked for it.
func (r *Runner) usageGuardFor(ctx context.Context, msg *queue.RunMessage, logger *iterlog.Logger) *usagecap.Guard {
	if !r.cfg.UsageCapPolicy.Enabled() {
		return nil
	}
	key := usageCapKey(ctx, msg)
	store := r.cfg.UsageCaps
	return usagecap.NewGuard(r.cfg.UsageCapPolicy, func(reading usagecap.Reading) {
		if store == nil {
			return
		}
		// Detached: the reading that stops a run arrives exactly as that
		// run's context is about to be cancelled, and it is the single
		// most valuable thing to publish.
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageCapStoreTimeout)
		defer cancel()
		if err := store.Record(wctx, key, reading); err != nil && logger != nil {
			// Best effort by design: an unpublished reading costs the next
			// pod a wasted call, never this run its correctness.
			logger.Warn("runner: publish usage reading (%s): %v", key, err)
		}
	})
}

// usageCapStoreTimeout bounds the shared-store round trips. Short: a wedged
// store must never hold a run's stream goroutine, and both callers degrade
// safely (publish is best effort, pre-flight fails open).
const usageCapStoreTimeout = 5 * time.Second

// usageCapPreflight reports the error a claimed run should fail with when
// the operator's cap leaves no headroom, or nil to proceed.
//
// It FAILS OPEN on every uncertainty — no policy, no store, an unreadable
// store, nothing measured yet, a reading whose window has since rolled over.
// A cap exists to protect a subscription from a fleet of bots, not to strand
// the fleet when its bookkeeping is unavailable: the mid-run guard is still
// armed behind it, so the worst case of failing open is one wasted call.
func (r *Runner) usageCapPreflight(ctx context.Context, wf *ir.Workflow, msg *queue.RunMessage, logger *iterlog.Logger) error {
	pol := r.cfg.UsageCapPolicy
	if !pol.Enabled() || r.cfg.UsageCaps == nil {
		return nil
	}
	// A workflow that cannot call a model has nothing to draw on the
	// subscription this cap protects. Blocking it would protect nothing and
	// lose whatever it was supposed to do meanwhile — for a feed collector,
	// material that no later run can recover, since a feed only serves a
	// short window. The mid-run guard stays armed either way.
	if !wf.UsesLLM() {
		if logger != nil {
			logger.Debug("runner: run %s makes no model call — usage cap not applied", msg.RunID)
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, usageCapStoreTimeout)
	defer cancel()
	key := usageCapKey(ctx, msg)
	readings, err := r.cfg.UsageCaps.Latest(rctx, key)
	if err != nil {
		if logger != nil {
			logger.Warn("runner: usage-cap pre-flight read (%s): %v — proceeding", key, err)
		}
		return nil
	}
	d := usagecap.Preflight(readings, pol, time.Now().UTC(), usagecap.DefaultMaxAge)
	if !d.Blocked {
		return nil
	}
	if logger != nil {
		logger.Warn("runner: run %s not started — %s", msg.RunID, d.Reason)
	}
	// The status flip is what makes the retry armable: ScheduleRunRetry
	// conditions on failed_resumable, and a run stopped before its first
	// node has never been marked anything. Without this the retry silently
	// fails to arm and the run is acked into nothing.
	if r.cfg.Store != nil {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), usageCapStoreTimeout)
		defer scancel()
		sctx = store.WithIdentity(sctx, msg.TenantID, msg.OwnerID)
		if _, serr := r.cfg.Store.UpdateRunStatusIf(sctx, msg.RunID, store.RunStatusFailedResumable,
			d.Reason,
			[]store.RunStatus{store.RunStatusRunning, store.RunStatusQueued}); serr != nil && logger != nil {
			logger.Warn("runner: usage-cap status flip for %s: %v", msg.RunID, serr)
		}
	}
	return &delegate.ErrRateLimited{
		Provider:    delegate.BackendClaudeCode,
		Detail:      fmt.Sprintf("%s (not started)", d.Reason),
		Kind:        delegate.RateLimitKindUsageWindow,
		ResetAt:     d.ResetsAt,
		SelfImposed: true,
	}
}
