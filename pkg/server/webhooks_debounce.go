package server

import (
	"context"
	"fmt"
	"net/http"
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

// deferSubjectKey scopes the debounce exactly like the supersede pass
// scopes cancellation: one pull request on one webhook.
func deferSubjectKey(cfg webhooks.Config, meta webhookEventMeta) string {
	return cfg.TenantID + "|" + cfg.ID + "|" + meta.SubjectID
}

// shouldDeferSyncLaunch reports whether this delivery rides the debounce:
// a synchronize-lane launch, with the store wired and a window configured.
func (s *Server) shouldDeferSyncLaunch(gateResync bool) bool {
	return gateResync && s.syncDebounce > 0 && s.webhookDeferred != nil
}

// deferSyncLaunch parks the resolved targets for the quiet window and
// answers the forge. The supersede pass runs NOW, not at fire time: a
// run already reviewing a head this push just obsoleted should stop
// burning tokens immediately.
func (s *Server) deferSyncLaunch(
	ctx context.Context,
	w http.ResponseWriter,
	cfg webhooks.Config,
	meta webhookEventMeta,
	targets []forgeLaunchTarget,
	payloadHash string,
	srcIP string,
) {
	for _, t := range targets {
		s.supersedeLiveRuns(ctx, cfg, meta, t.BotID)
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
	}
	for _, t := range targets {
		d.Targets = append(d.Targets, webhooks.DeferredTarget{
			BotID: t.BotID, IdemKey: t.IdemKey, Vars: t.Vars,
			RepoURL: t.RepoURL, RepoRef: t.RepoRef,
		})
	}
	if err := s.webhookDeferred.Upsert(ctx, d); err != nil {
		// The park failed — launching immediately is strictly better than
		// dropping the review (the pre-debounce behaviour, and the forge
		// has already been promised a review of this head).
		if s.logger != nil {
			s.logger.Warn("webhooks: defer of %s %s failed (%v) — launching immediately instead", cfg.ID, meta.SubjectID, err)
		}
		s.insertAndLaunchWebhookMulti(ctx, w, nil, cfg, meta, targets, payloadHash, srcIP)
		return
	}
	s.markWebhookOutcome(cfg.Provider, webhooks.StatusDeferred)
	if s.logger != nil {
		s.logger.Info("webhooks: %s/%s %s deferred %s — review launches after a %s quiet window (a newer push re-arms it)",
			cfg.Provider, meta.ProjectPath, meta.SubjectID, meta.SubjectSHA, s.syncDebounce)
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": webhooks.StatusDeferred})
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
		s.fireDeferredWebhookLaunch(ctx, d)
	}
}

// fireDeferredWebhookLaunch replays one parked delivery through the
// ordinary launch tail and acknowledges the row's generation. Every
// outcome — launched, duplicate, denial, error — acknowledges: the tail
// has recorded its own delivery row either way, and re-firing a denial
// would re-deny every 20s until the TTL.
func (s *Server) fireDeferredWebhookLaunch(ctx context.Context, d webhooks.DeferredLaunch) {
	cfg, err := s.webhookConfigs.Get(ctx, d.WebhookID)
	if err != nil {
		// The webhook is gone (deleted between park and fire) — the row
		// can never launch. Drop it rather than re-claim it forever.
		s.warnf("webhook debounce sweeper: config %s gone (%v) — dropping parked launch for %s", d.WebhookID, err, d.SubjectID)
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
	meta := webhookEventMeta{
		Kind:         d.EventKind,
		Action:       d.EventAction,
		ProjectPath:  d.ProjectPath,
		SubjectID:    d.SubjectID,
		SubjectURL:   d.SubjectURL,
		SubjectSHA:   d.SubjectSHA,
		SenderHandle: d.SenderHandle,
	}
	for _, t := range d.Targets {
		res := s.launchWebhookTarget(ctx, nil, cfg, meta, forgeLaunchTarget{
			BotID: t.BotID, IdemKey: t.IdemKey, Vars: t.Vars,
			RepoURL: t.RepoURL, RepoRef: t.RepoRef,
		}, d.PayloadHash, d.SourceIP)
		if res.Status == webhooks.StatusLaunched {
			s.scheduleForgeBoardProjection(meta.ProjectPath)
		}
		if s.logger != nil && res.Status != webhooks.StatusLaunched && res.Status != webhooks.StatusDuplicate {
			s.logger.Warn("webhook debounce sweeper: parked %s launch for %s ended %s: %s", t.BotID, d.SubjectID, res.Status, res.Error)
		}
	}
	_ = s.webhookDeferred.Delete(ctx, d.SubjectKey, d.Generation)
}
