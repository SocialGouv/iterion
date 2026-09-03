package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Push debounce for the synchronize review lane.
//
// A developer polishing a PR pushes in volleys. Every push used to launch a
// full review immediately, and the NEXT push cancelled it mid-flight
// (supersede) — measured on 2026-09-03: ~18% of a morning's review runs were
// "superseded by a newer delivery", each having already burned minutes of a
// reviewer's tokens before dying. The debounce parks the launch instead: a
// synchronize delivery waits for a quiet window, a newer push on the same
// subject replaces the parked payload and pushes the window back, and only
// the FINAL head gets its review.
//
// Scope: ONLY the synchronize lane (a push to an already-reviewed PR). A
// PR open, a `/revi` command and a re-request click stay immediate — those
// are deliberate human gestures waiting for an answer. The supersede pass
// remains the net for a run already in flight when a push lands.
//
// The gate's required check simply stays ABSENT during the window — the
// honest state for "nothing is reviewing this yet", exactly what the
// seconds between push and launch look like today. Deliberately NO
// in-flight claim at deferral: a claim must name the run that owes it
// (forge_gate_pending.go's attribution rule), and a parked launch has no
// run — an ownerless pending dropped by a restart would block the PR with
// nothing to reconcile it.

const (
	// webhookSyncDebounceDefault is the quiet window when the env does not
	// say otherwise. Sized to cover a "push, notice a typo, push again"
	// volley without holding a legitimate lone push's verdict long.
	webhookSyncDebounceDefault = 3 * time.Minute

	// webhookDeferSweepInterval is the added latency ceiling on top of the
	// window itself.
	webhookDeferSweepInterval = 20 * time.Second

	// webhookDeferLease bounds how long a claimed row is invisible to the
	// other replicas. A claimer that dies mid-launch lets it lapse and the
	// row re-fires; the launch tail's idempotency key makes the retry of an
	// already-launched target a duplicate no-op.
	webhookDeferLease = 2 * time.Minute

	webhookDeferBatch = 50

	// Retry chain for a parked row the launch tail refused (see
	// deferRetryWait). webhookDeferRetryBase doubles per attempt up to
	// webhookDeferRetryMax; webhookDeferMaxAttempts caps the chain
	// (~45 min of retries), and a denial whose reset lies past
	// webhookDeferRetryHorizon is not worth waiting for at all.
	webhookDeferRetryBase    = 30 * time.Second
	webhookDeferRetryMax     = 10 * time.Minute
	webhookDeferMaxAttempts  = 8
	webhookDeferRetryHorizon = 30 * time.Minute
)

// webhookSyncDebounceFromEnv resolves the quiet window.
// ITERION_WEBHOOK_SYNC_DEBOUNCE accepts a Go duration ("3m", "45s");
// "0" (or any zero/negative duration) disables the debounce. An
// unparsable value falls back to the default with a warning on stderr —
// failing open (no debounce) would silently restore the waste this
// exists to cut, failing closed (huge window) would silently delay
// verdicts; the default is the only value the operator has read about.
func webhookSyncDebounceFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("ITERION_WEBHOOK_SYNC_DEBOUNCE"))
	if raw == "" {
		return webhookSyncDebounceDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iterion: ITERION_WEBHOOK_SYNC_DEBOUNCE=%q is not a duration (want e.g. \"3m\", \"0\" to disable) — using the default %s\n",
			raw, webhookSyncDebounceDefault)
		return webhookSyncDebounceDefault
	}
	if d <= 0 {
		return 0
	}
	return d
}

// deferSubjectKey scopes the debounce to ONE pull request of ONE project
// on one webhook. The project path is load-bearing: a subject id ("pr:7")
// carries no repo and one webhook config routinely serves many
// (ProjectAllowlist, an org-level hook), so keying on the subject alone
// would let a push to acme/b#7 REPLACE the parked review of acme/a#7 —
// which would then never launch and never retry (no delivery row was
// ever written).
func deferSubjectKey(cfg webhooks.Config, meta webhookEventMeta) string {
	return cfg.TenantID + "|" + cfg.ID + "|" + meta.ProjectPath + "|" + meta.SubjectID
}

// shouldDeferSyncLaunch reports whether this delivery rides the debounce:
// a synchronize-lane launch, with the store wired and a window configured.
func (s *Server) shouldDeferSyncLaunch(gateResync bool) bool {
	return gateResync && s.syncDebounce > 0 && s.webhookDeferred != nil
}

// dropReplayedTargets removes the targets whose idempotency key already
// names a non-retryable delivery — launchWebhookTarget's step-1 replay
// check, hoisted so the DEFER lane can apply it in the same order the
// immediate lane does (replay BEFORE supersede). A prior
// StatusLaunchError stays retryable there and here.
func (s *Server) dropReplayedTargets(ctx context.Context, targets []forgeLaunchTarget) []forgeLaunchTarget {
	if s.webhookDeliveries == nil {
		return targets
	}
	fresh := make([]forgeLaunchTarget, 0, len(targets))
	for _, t := range targets {
		if prior, err := s.webhookDeliveries.GetByIdempotencyKey(ctx, t.IdemKey); err == nil &&
			prior.Status != webhooks.StatusLaunchError {
			continue
		}
		fresh = append(fresh, t)
	}
	return fresh
}

// deferSyncLaunch parks the resolved targets for the quiet window and
// answers the forge. The supersede pass runs NOW, not at fire time: a
// run already reviewing a head this push just obsoleted should stop
// burning tokens immediately — but only once the store has ACCEPTED
// this payload as the current one, so a late delivery of an older head
// cannot cancel the live review of a newer one.
func (s *Server) deferSyncLaunch(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	cfg webhooks.Config,
	meta webhookEventMeta,
	targets []forgeLaunchTarget,
	payloadHash string,
	srcIP string,
) {
	// Replay guard FIRST, exactly as launchWebhookTarget orders it (step 1
	// replay, step 1b supersede). A REDELIVERY of a synchronize whose
	// parked launch already fired — an operator "Redeliver", a lost ack —
	// would otherwise cancel the very run it launched, then re-park under
	// the same idempotency key, which the sweep answers `duplicate`: the
	// review is dead and nothing relaunches it, leaving the required check
	// absent forever.
	targets = s.dropReplayedTargets(ctx, targets)
	if len(targets) == 0 {
		s.markWebhookOutcome(cfg.Provider, webhooks.StatusDuplicate)
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusDuplicate})
		return
	}
	d := webhooks.DeferredLaunch{
		SubjectKey:   deferSubjectKey(cfg, meta),
		TenantID:     cfg.TenantID,
		WebhookID:    cfg.ID,
		FireAt:       time.Now().UTC().Add(s.syncDebounce),
		CreatedAt:    time.Now().UTC(),
		EventKind:    meta.Kind,
		EventAction:  meta.Action,
		ProjectPath:  meta.ProjectPath,
		SubjectID:    meta.SubjectID,
		SubjectURL:   meta.SubjectURL,
		SubjectSHA:   meta.SubjectSHA,
		SenderHandle: meta.SenderHandle,
		PayloadHash:  payloadHash,
		SourceIP:     srcIP,
		// The forge's own timestamp orders two payloads for one subject.
		// Arrival order cannot: forges do not guarantee delivery order,
		// and pushes seconds apart are this feature's whole regime.
		OrderKey: meta.EventUpdatedAt,
		// Mirror the request-derived server base: on a deployment with no
		// PublicURL the launch tail resolves the forge publish grant's
		// endpoint from the inbound request, which the sweep won't have.
		PublicBase: s.publicBaseURL(r),
	}
	for _, t := range targets {
		d.Targets = append(d.Targets, webhooks.DeferredTarget{
			BotID: t.BotID, IdemKey: t.IdemKey, Vars: t.Vars,
			RepoURL: t.RepoURL, RepoRef: t.RepoRef,
		})
	}
	accepted, err := s.webhookDeferred.Upsert(ctx, d)
	if err != nil {
		// The park failed — launching immediately is strictly better than
		// dropping the review (the pre-debounce behaviour, and the forge
		// has already been promised a review of this head). The inbound
		// request rides along: the denial path writes CORS headers off it.
		// insertAndLaunchWebhookMulti runs the supersede pass itself, which
		// is why this lane's own supersede sits below, on the parked path.
		if s.logger != nil {
			s.logger.Warn("webhooks: defer of %s %s failed (%v) — launching immediately instead", cfg.ID, meta.SubjectID, err)
		}
		s.insertAndLaunchWebhookMulti(ctx, w, r, cfg, meta, targets, payloadHash, srcIP)
		return
	}
	if !accepted {
		// A strictly NEWER push is already parked for this subject: this
		// delivery arrived out of order (a forge retry, a slow dispatch).
		// Nothing to do — and emphatically no supersede, which would
		// cancel the live review of a head this one predates.
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, payloadHash, srcIP,
			"out-of-order delivery: a newer push on this subject is already parked")
		if s.logger != nil {
			s.logger.Info("webhooks: %s/%s %s ignored %s — a newer push is already parked (delivery arrived out of order)",
				cfg.Provider, meta.ProjectPath, meta.SubjectID, meta.SubjectSHA)
		}
		writeJSONStatus(w, http.StatusOK, map[string]string{"status": webhooks.StatusFiltered})
		return
	}
	// Only NOW, with this payload accepted as the subject's current one:
	// a run reviewing a head this push just obsoleted stops burning
	// tokens immediately.
	for _, t := range targets {
		s.supersedeLiveRuns(ctx, cfg, meta, t.BotID)
	}
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusDeferred)
	if s.logger != nil {
		s.logger.Info("webhooks: %s/%s %s deferred %s — review launches after a %s quiet window (a newer push re-arms it)",
			cfg.Provider, meta.ProjectPath, meta.SubjectID, meta.SubjectSHA, s.syncDebounce)
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": webhooks.StatusDeferred})
}

// syntheticRequestForBase rebuilds the minimal *http.Request
// publicBaseURL needs to re-derive a stored "scheme://host" base — the
// deferred lane's stand-in for the inbound request it no longer has.
// Returns nil for an empty/unparsable base (publicBaseURL then falls
// back to cfg.PublicURL alone, the pre-fix behaviour).
func syntheticRequestForBase(base string) *http.Request {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil
	}
	req := &http.Request{Host: u.Host, Header: http.Header{}, URL: &url.URL{}}
	if u.Scheme == "https" {
		// publicBaseURL reads scheme off r.TLS, not the URL.
		req.TLS = &tls.ConnectionState{}
	}
	return req
}

// runWebhookDeferSweeper ticks until ctx is cancelled, launching parked
// deliveries whose quiet window elapsed. Started by ListenAndServe when
// the deferred store is wired; multi-replica-safe via the store lease.
func (s *Server) runWebhookDeferSweeper(ctx context.Context) {
	s.infof("webhook debounce sweeper: launching parked synchronize reviews after their %s quiet window (sweep every %s)",
		s.syncDebounce, webhookDeferSweepInterval)
	t := time.NewTicker(webhookDeferSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepDeferredWebhookLaunches(ctx, time.Now().UTC())
		}
	}
}

// sweepDeferredWebhookLaunches performs one pass. Extracted (with an
// injectable clock) for tests.
func (s *Server) sweepDeferredWebhookLaunches(ctx context.Context, now time.Time) {
	if s.webhookDeferred == nil {
		return
	}
	due, err := s.webhookDeferred.ClaimDue(ctx, now, webhookDeferLease, webhookDeferBatch)
	if err != nil {
		s.warnf("webhook debounce sweeper: claim: %v", err)
		return
	}
	for _, d := range due {
		s.fireDeferredWebhookLaunch(ctx, d, now)
	}
}

// fireDeferredWebhookLaunch replays one parked delivery through the
// ordinary launch tail. A row that LAUNCHED (or replayed as a duplicate)
// is acknowledged by generation; one that did not launch is re-armed —
// see deferRetryWait for why, and for the horizon past which the loss
// becomes terminal and audited.
func (s *Server) fireDeferredWebhookLaunch(ctx context.Context, d webhooks.DeferredLaunch, now time.Time) {
	cfg, err := s.webhookConfigs.Get(ctx, d.WebhookID)
	if errors.Is(err, webhooks.ErrNotFound) {
		// The webhook is gone (deleted between park and fire) — the row
		// can never launch. Drop it rather than re-claim it forever.
		s.warnf("webhook debounce sweeper: config %s gone — dropping parked launch for %s", d.WebhookID, d.SubjectID)
		_ = s.webhookDeferred.Delete(ctx, d.SubjectKey, d.Generation)
		return
	}
	if err != nil {
		// A transient store error (timeout, decode, server selection) is
		// NOT "the webhook is gone": keep the row, let the lease lapse
		// and the next sweep retry — deleting here would silently lose a
		// review the forge was promised.
		s.warnf("webhook debounce sweeper: config %s unreadable (%v) — will retry parked launch for %s", d.WebhookID, err, d.SubjectID)
		return
	}
	meta := webhookEventMeta{
		Kind:         d.EventKind,
		Action:       d.EventAction,
		ProjectPath:  d.ProjectPath,
		SubjectID:    d.SubjectID,
		SubjectURL:   d.SubjectURL,
		SubjectSHA:   d.SubjectSHA,
		SenderHandle: d.SenderHandle,
	}
	// The config as it stands NOW governs, not the copy read when the
	// push landed. The quiet window is real time an operator can act in,
	// and the sweep bypasses every admission the inbound request passed
	// through — the `!Enabled → 410` guard lives in the auth middleware
	// (middleware_webhook.go), which a replay never re-enters. Letting a
	// parked row outlive the switch that turns it off would silently
	// replace an operator's explicit choice (CLAUDE.md principle 1).
	// ReviewOnSync is the same call: every parked row IS a synchronize
	// re-review (the only lane that defers), and clearing it is a
	// deliberate repo-wide posture change the provisioner already logs
	// as definitive. Both drop the row: the forge was ACKed minutes ago,
	// so there is no retry to preserve, and "the operator said no" is a
	// terminal answer, not a transient one.
	if !cfg.Enabled || !cfg.ReviewOnSync {
		why := "webhook disabled during the quiet window"
		if cfg.Enabled {
			why = "review_on_sync cleared during the quiet window"
		}
		s.warnf("webhook debounce sweeper: dropping parked launch for %s %s — %s", d.ProjectPath, d.SubjectID, why)
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusFiltered, d.PayloadHash, d.SourceIP,
			"parked review dropped: "+why)
		_ = s.webhookDeferred.Delete(ctx, d.SubjectKey, d.Generation)
		return
	}
	// Re-stamp the synthetic webhook identity the auth middleware put on
	// the original request's context: the launch tail's admission gate
	// (gateLaunch) meters BY it, and a bare background context would be
	// denied as teamless.
	actor := "webhook:" + cfg.ID
	ctx = auth.WithIdentity(ctx, auth.Identity{
		UserID: actor,
		TeamID: cfg.TenantID,
		Role:   identity.RoleMember,
		Kind:   auth.KindWebhook,
	})
	ctx = store.WithIdentity(ctx, cfg.TenantID, actor)
	// Rebuild the base-URL carrier the launch tail expects: with no
	// PublicURL configured, injectForgePublishVars derives the publish
	// endpoint from the request's Host — a synthetic request carrying the
	// base mirrored at defer time keeps the deferred lane at parity with
	// the immediate one (it is ONLY read by publicBaseURL; no handler
	// writes a response through it).
	req := syntheticRequestForBase(d.PublicBase)
	var (
		failed string        // non-empty = nothing launched for this target
		denial *launchDenial // the admission refusal, when that is why
	)
	for _, t := range d.Targets {
		// Bot scope is re-read too: a bot removed from BotIDs while the
		// row sat parked must not launch. Per-target, not per-row — a
		// fan-out's other bots are still in scope.
		if !cfg.AllowsBot(t.BotID) {
			s.warnf("webhook debounce sweeper: dropping parked %s launch for %s — bot no longer in the webhook's scope", t.BotID, d.SubjectID)
			continue
		}
		res := s.launchWebhookTarget(ctx, req, cfg, meta, forgeLaunchTarget{
			BotID: t.BotID, IdemKey: t.IdemKey, Vars: t.Vars,
			RepoURL: t.RepoURL, RepoRef: t.RepoRef,
		}, d.PayloadHash, d.SourceIP)
		if res.Status == webhooks.StatusLaunched {
			s.scheduleForgeBoardProjection(meta.ProjectPath)
		}
		if res.Status == webhooks.StatusLaunched || res.Status == webhooks.StatusDuplicate {
			continue
		}
		s.warnf("webhook debounce sweeper: parked %s launch for %s ended %s: %s", t.BotID, d.SubjectID, res.Status, res.Error)
		failed = t.BotID + ": " + res.Error
		if res.denial != nil {
			// Denials are org-scoped (concurrency, launch rate, monthly
			// quota, cost cap) — the remaining bots would deny identically
			// and each re-record the same terminal row, as the immediate
			// lane's fan-out already breaks for.
			denial = res.denial
			break
		}
	}
	if failed != "" {
		if wait, ok := deferRetryWait(denial, d.Attempts); ok {
			// NOT delete: the forge was answered `202 deferred` minutes ago,
			// so it will never redeliver, and the immediate lane's recovery —
			// hand the denial's 4xx back and let the forge retry — does not
			// exist here. Re-arm on the denial's own horizon instead.
			// Generation-guarded, so a push that landed mid-launch keeps its
			// fresher payload.
			if err := s.webhookDeferred.Reschedule(ctx, d.SubjectKey, d.Generation, now.Add(wait), d.Attempts+1); err != nil {
				// Leave the row alone: its lease lapses and the next sweep
				// re-fires it, which is the same at-least-once net a dead
				// claimer relies on.
				s.warnf("webhook debounce sweeper: could not re-arm parked launch for %s (%v) — the lapsing lease will re-fire it", d.SubjectID, err)
				return
			}
			s.warnf("webhook debounce sweeper: parked launch for %s %s did not launch (%s) — retrying in %s (attempt %d/%d)",
				d.ProjectPath, d.SubjectID, failed, wait, d.Attempts+1, webhookDeferMaxAttempts)
			return
		}
		// Terminal: name the loss instead of deleting the row in silence.
		// A monthly quota/cost reset is weeks out, and by then this head
		// is long stale — an honest terminal loss beats a row that
		// no-ops for a month.
		s.warnf("webhook debounce sweeper: ABANDONING the parked review of %s %s after %d attempt(s) — %s",
			d.ProjectPath, d.SubjectID, d.Attempts+1, failed)
		s.recordTerminalWebhookDelivery(ctx, cfg, meta, webhooks.StatusLaunchError, d.PayloadHash, d.SourceIP,
			fmt.Sprintf("parked review abandoned after %d attempt(s): %s", d.Attempts+1, failed))
	}
	_ = s.webhookDeferred.Delete(ctx, d.SubjectKey, d.Generation)
}

// deferRetryWait decides whether a parked row that failed to launch
// should try again, and after how long. Reported false = terminal, drop
// it (loudly, with an audit row).
//
// The immediate lane recovers from a launch denial by handing the
// forge the denial's own 4xx/5xx: the delivery shows failed and a
// retry launches once the quota resets. The deferred lane forfeits
// that — the forge was ACKed `202 deferred` minutes ago — so the retry
// has to live here or the promised review is simply lost.
//
// The chain is bounded on two axes. Attempts caps the count; and a
// denial that names a reset FURTHER OUT than the chain's own cadence
// (today only the monthly run-quota / cost-cap denials, whose resetAt
// is the next month) is refused outright: parking a row for weeks buys
// nothing — the head it reviews is stale long before then — and hides
// the loss behind a row that will silently no-op.
func deferRetryWait(denial *launchDenial, attempts int) (time.Duration, bool) {
	if attempts+1 >= webhookDeferMaxAttempts {
		return 0, false
	}
	if denial != nil && !denial.resetAt.IsZero() &&
		time.Until(denial.resetAt) > webhookDeferRetryHorizon {
		return 0, false
	}
	// Exponential from the sweep cadence, floored by the denial's own
	// Retry-After hint (concurrency 30s, launch rate its bucket's), capped.
	wait := webhookDeferRetryBase << min(attempts, 8)
	if denial != nil && denial.retryAfter > wait {
		wait = denial.retryAfter
	}
	return min(wait, webhookDeferRetryMax), true
}
