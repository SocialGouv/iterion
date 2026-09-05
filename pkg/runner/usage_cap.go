package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
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

// runCredKeys is the per-run credential identity the meter draws on: the
// scope (tenant-own vs platform) plus one fingerprint per credential SHAPE
// the bundle holds. A reading is keyed by the shape the node actually
// exercised — the delegate stamps its provider-routing label on each
// Reading (usagecap.Reading.Source) — because a bundle may carry both a
// z.ai token and an Anthropic key, and a node pinned `provider: anthropic`
// spends the Anthropic key while the bundle-default precedence points at
// z.ai. Charging that refusal to the z.ai fingerprint would make the
// evidence-based skip park the healthy key and keep the frozen one.
type runCredKeys struct {
	scope       string
	zaiFP       string
	anthropicFP string
	oauthFP     string
}

// usageCapCredKeys reads the run's resolved credentials once. Scope: a
// bundle carrying any credential the TENANT resolved is the tenant's own;
// anything else shares the cross-tenant meter — a slot the publisher
// filled from the DB-backed platform tier (the deployment's single
// subscription) and a slot the credential POOL filled with a contributor's
// lent one (the donor's single subscription, borrowed by several tenants
// in turn). Both ride the bundle exactly like a tenant credential and
// neither is one; metering a lent credential per borrower would open one
// ledger per borrower of the SAME account, so what one of them measured —
// a refusal, a window at 95% — would reach none of the others.
func usageCapCredKeys(ctx context.Context, msg *queue.RunMessage) runCredKeys {
	k := runCredKeys{scope: usagecap.ScopePlatform}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return k
	}
	tenantOwnZai := creds.APIKey(secrets.ProviderZAI) != "" &&
		creds.IsTenantOwned(string(secrets.ProviderZAI))
	tenantOwnKey := creds.APIKey(secrets.ProviderAnthropic) != "" &&
		creds.IsTenantOwned(string(secrets.ProviderAnthropic))
	tenantOwnOAuth := creds.OAuthDir(delegate.BackendClaudeCode) != "" &&
		creds.IsTenantOwned(delegate.BackendClaudeCode)
	if tenantOwnZai || tenantOwnKey || tenantOwnOAuth {
		k.scope = usagecap.TenantScope(msg.TenantID)
	}
	k.zaiFP = creds.Fingerprint(string(secrets.ProviderZAI))
	k.anthropicFP = creds.Fingerprint(string(secrets.ProviderAnthropic))
	k.oauthFP = creds.Fingerprint(delegate.BackendClaudeCode)
	return k
}

// forSource keys a reading under the credential its session actually ran
// on. The source labels are providerFingerprint's vocabulary: a facade URL
// is the z.ai token, "anthropic-direct" the Anthropic API key,
// "anthropic-oauth" the OAuth dir. An empty label (older binary) and
// "anthropic-env" (inherited pod env — no bundle credential at all) fall
// back to the bundle-default precedence: z.ai AUTH_TOKEN over an Anthropic
// API key over an OAuth dir, anthropicCredEnvForCLI's contract. A rotated
// token therefore opens a fresh meter instead of inheriting the readings
// of the account it replaced.
func (k runCredKeys) forSource(source string) string {
	fp := ""
	switch {
	case strings.HasPrefix(source, "facade:") && k.zaiFP != "":
		fp = k.zaiFP
	case source == "anthropic-direct" && k.anthropicFP != "":
		fp = k.anthropicFP
	case source == "anthropic-oauth" && k.oauthFP != "":
		fp = k.oauthFP
	default:
		if k.zaiFP != "" {
			fp = k.zaiFP
		} else if k.anthropicFP != "" {
			fp = k.anthropicFP
		} else if k.oauthFP != "" {
			fp = k.oauthFP
		}
	}
	return usagecap.Key(delegate.BackendClaudeCode, k.scope, fp)
}

// usageCapKey is the run's DEFAULT credential key — what the pre-flight
// consults before any node has run (no session, so no source label yet).
func usageCapKey(ctx context.Context, msg *queue.RunMessage) string {
	return usageCapCredKeys(ctx, msg).forSource("")
}

// usageGuardFor builds the guard for one run: the machine-wide policy
// SOURCE, with every reading published to the shared store under the run's
// credential key. The guard re-reads the source per evaluation, so a cap
// tightened at runtime (the DB-backed settings record) bites a run already
// in flight — which is also why a LIVE source that answers "nothing capped
// right now" still gets a guard: the answer can change before the run
// ends.
//
// RECORDING IS NOT ENFORCING, and the guard is the only path either
// travels. A deployment that configured no cap still needs the provider's
// refusals on the shared ledger: the credential-tier skips (a frequency
// refusal, a rejected credential) read nothing else, and route the next
// run around a credential the provider will not serve. Gating the guard on
// the cap policy made every one of them inert on exactly the deployments
// that never asked for a ceiling. So a ledger alone is reason enough; only
// with neither a policy source nor a store — nothing to enforce, nobody to
// tell — is there no guard.
func (r *Runner) usageGuardFor(ctx context.Context, msg *queue.RunMessage, logger *iterlog.Logger) *usagecap.Guard {
	src := r.cfg.UsageCapSource
	store := r.cfg.UsageCaps
	// A static policy with no cap can never block; a live source can start
	// blocking mid-run, so it always counts as enforceable.
	enforceable := src != nil
	if pol, static := src.(usagecap.StaticPolicy); static && !usagecap.Policy(pol).Enabled() {
		enforceable = false
	}
	if !enforceable && store == nil {
		return nil
	}
	if src == nil {
		// Inert policy: the guard observes and publishes, and blocks nothing.
		src = usagecap.StaticPolicy(usagecap.Policy{})
	}
	keys := usageCapCredKeys(ctx, msg)
	return usagecap.NewGuardWithSource(src, func(reading usagecap.Reading) {
		if store == nil {
			return
		}
		// Keyed per reading, not per run: the session's provider-routing
		// label names the credential the node actually spent, and a run
		// whose nodes pin different providers must not charge one key's
		// refusal to another's meter.
		key := keys.forSource(reading.Source)
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
	// The cap meters the Anthropic wire — its readings come from the
	// claude_code delegate and nowhere else — and the key below is built
	// from the run's anthropic-wire credentials (or the platform's). A run
	// whose every route is pinned off that wire (claw/openai, codex) can
	// never spend what the cap protects: parking it for the anthropic
	// weekly reset strands it for nothing, which is how a fully pinned
	// two-node rite froze for five days while its single-node sibling
	// sailed through (#668). Read under the launch's own overrides, on
	// PRIMARY routes only — a rescue `fallbacks:` route onto the wire
	// fires on a failure the mid-run guard already refuses, and cannot
	// justify refusing the run before it starts. Every uncertainty
	// answers "reachable".
	if !model.AnthropicWireReachable(wf, modelOverridesFromMsg(msg.ModelOverrides)) {
		if logger != nil {
			logger.Debug("runner: run %s targets no anthropic-wire route — usage cap not applied", msg.RunID)
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
	d := usagecap.Preflight(readings, pol, time.Now().UTC(), r.cfg.UsageCapTrust)
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
		if _, serr := r.cfg.Store.UpdateRunOutcome(sctx, msg.RunID, store.RunStatusFailedResumable,
			d.Reason,
			// Continuation deliberately unknown here: the arming
			// decision happens later (armUsageWindowRetry), and three
			// of its branches arm nothing — the document must not say
			// retry_armed before a retry actually exists. Promotion to
			// retry_armed lives with ScheduleRunRetry; demotion to
			// final with AbandonRunRetry.
			store.RunOutcomeMeta{Code: store.FailureUsageLimitBlocked},
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

// admitAttempt is the last gate before an attempt can spend anything, and
// the moment it takes hold of what it will spend. The pre-flight decides
// whether the run may start at all; only once it has, the attempt stamps
// its credentials as held, so a multi-hour attempt does not read as an
// idle key for its whole duration (#659 pt 2).
//
// The order is the point, both ways: a run parked on a ceiling never held
// anything (stamping it would date a key that served nothing), and a run
// that starts must not wait until it ends to say which key it is spending.
func (r *Runner) admitAttempt(ctx context.Context, wf *ir.Workflow, msg *queue.RunMessage) error {
	if err := r.usageCapPreflight(ctx, wf, msg, r.cfg.Logger); err != nil {
		return err
	}
	r.markCredFingerprintsUsed(ctx, msg, time.Now().UTC())
	return nil
}
