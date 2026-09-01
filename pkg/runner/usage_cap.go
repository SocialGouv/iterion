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
// OAuth dir the TENANT resolved is the tenant's own; anything else —
// including a bundle slot the publisher filled from the DB-backed platform
// tier, which rides the bundle exactly like a tenant credential but is the
// deployment's single meter — is the platform's.
func usageCapKey(ctx context.Context, msg *queue.RunMessage) string {
	scope := usagecap.ScopePlatform
	credFP := ""
	if creds, ok := secrets.CredentialsFromContext(ctx); ok {
		tenantOwnKey := creds.APIKey(secrets.ProviderAnthropic) != "" &&
			!creds.IsPlatformSourced(string(secrets.ProviderAnthropic))
		tenantOwnOAuth := creds.OAuthDir(delegate.BackendClaudeCode) != "" &&
			!creds.IsPlatformSourced(delegate.BackendClaudeCode)
		if tenantOwnKey || tenantOwnOAuth {
			scope = usagecap.TenantScope(msg.TenantID)
		}
		// The meter follows the CREDENTIAL: same preference order as the
		// delegate (a ctx API key outranks an OAuth dir), so the
		// fingerprint names the credential the run will actually spend.
		// A rotated token therefore opens a fresh meter instead of
		// inheriting the readings of the account it replaced.
		if fp := creds.Fingerprint(string(secrets.ProviderAnthropic)); fp != "" {
			credFP = fp
		} else if fp := creds.Fingerprint(delegate.BackendClaudeCode); fp != "" {
			credFP = fp
		}
	}
	return usagecap.Key(delegate.BackendClaudeCode, scope, credFP)
}

// usageGuardFor builds the guard for one run: the machine-wide policy
// SOURCE, with every reading published to the shared store under the run's
// credential key. The guard re-reads the source per evaluation, so a cap
// tightened at runtime (the DB-backed settings record) bites a run already
// in flight — which is also why a LIVE source that answers "nothing capped
// right now" still gets a guard: the answer can change before the run
// ends. Only when no cap could ever apply — no source at all, or a STATIC
// policy with no cap — is the guard skipped, keeping the feature out of
// the way of a deployment that never asked for it.
func (r *Runner) usageGuardFor(ctx context.Context, msg *queue.RunMessage, logger *iterlog.Logger) *usagecap.Guard {
	if r.cfg.UsageCapSource == nil {
		return nil
	}
	if pol, static := r.cfg.UsageCapSource.(usagecap.StaticPolicy); static && !usagecap.Policy(pol).Enabled() {
		return nil
	}
	key := usageCapKey(ctx, msg)
	store := r.cfg.UsageCaps
	return usagecap.NewGuardWithSource(r.cfg.UsageCapSource, func(reading usagecap.Reading) {
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
	if r.cfg.UsageCapSource == nil || r.cfg.UsageCaps == nil {
		return nil
	}
	// The LIVE effective policy — env defaults + the runtime settings
	// record, one TTL-bounded lookup per claimed run.
	pol := r.cfg.UsageCapSource.Effective(ctx)
	if !pol.Enabled() {
		return nil
	}
	// Refuse in advance only what could not possibly avoid spending. A
	// workflow with any model-free path — the collect half of a two-mode
	// feed bot, say — is let through and stopped by the MID-RUN guard if it
	// actually reaches a model call. Blocking it here protects nothing and
	// loses what it was there to do: for a collector, material no later run
	// recovers, since a feed serves a short window and does not remember
	// what nobody fetched.
	if !wf.AlwaysReachesLLM() {
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
		if _, serr := r.cfg.Store.UpdateRunStatusIfCoded(sctx, msg.RunID, store.RunStatusFailedResumable,
			d.Reason, store.FailureUsageLimitBlocked,
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
