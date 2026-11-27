package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

type webhookCtxKey struct{}

// webhookActor is the synthetic identity every webhook lane acts under: the
// run's owner, the store identity that scopes its writes, and — the reason
// this is one name rather than four spellings — the user id secrets.Resolve
// measures a credential's visibility against. No BYOK key is ever scoped to
// it, so what it may spend is exactly the team-scoped set
// (secrets.SpendableBy).
func webhookActor(configID string) string { return "webhook:" + configID }

// webhookConfigFromContext returns the authenticated webhook Config a
// provider handler should act on. Set by webhookAuth.
func webhookConfigFromContext(ctx context.Context) (webhooks.Config, bool) {
	c, ok := ctx.Value(webhookCtxKey{}).(webhooks.Config)
	return c, ok
}

type teamCtxKey struct{}

// withTeam stashes a freshly-loaded Team on ctx so downstream gates
// (gateLaunch) can skip re-fetching the same document — the webhook
// path otherwise pays two Mongo round trips per delivery for one team.
func withTeam(ctx context.Context, t identity.Team) context.Context {
	return context.WithValue(ctx, teamCtxKey{}, t)
}

// teamFromContext returns the pre-loaded Team when one was stashed.
func teamFromContext(ctx context.Context) (identity.Team, bool) {
	t, ok := ctx.Value(teamCtxKey{}).(identity.Team)
	return t, ok
}

// extractWebhookToken pulls the presented token from the provider's
// native header, falling back to iterion's own header. Only invoked
// in SignModeToken — hmac-mode providers (GitHub, Forgejo) DO NOT
// echo a token header at all, so the middleware skips this entirely
// (see webhookAuth).
func extractWebhookToken(r *http.Request, provider webhooks.Provider) string {
	if provider == webhooks.ProviderGitLab {
		if t := r.Header.Get("X-Gitlab-Token"); t != "" {
			return t
		}
	}
	return r.Header.Get("X-Iterion-Webhook-Token")
}

// webhookHMACSignature returns the body signature an hmac-mode provider
// presents, read from that provider's header. The one place the header
// names live: the middleware and the provider handlers verify the same
// value. Empty for a provider that never signs its body, which no
// signature verifies.
func webhookHMACSignature(provider webhooks.Provider, r *http.Request) string {
	switch provider {
	case webhooks.ProviderGitHub:
		return r.Header.Get("X-Hub-Signature-256")
	case webhooks.ProviderForgejo:
		return forgejoSignatureHeader(r)
	case webhooks.ProviderGeneric:
		return r.Header.Get("X-Iterion-Webhook-Signature")
	default:
		return ""
	}
}

// webhookAuth is the inbound-webhook authentication + admission
// middleware. It is NOT chained from requireAuth — /api/webhooks/* is
// public to the JWT layer (see isPublicPath) and authenticates itself.
//
// Order (everything before the handler does any work): resolve config by
// URL id → authenticate (the constant-time token check, or the HMAC over
// the bounded body) → enabled → per-webhook rate limit → per-org monthly
// quota → org not suspended → stamp tenant identity + the config on ctx.
// A failure at any step short-circuits with the right status and never
// reaches the provider handler.
func (s *Server) webhookAuth(provider webhooks.Provider, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.webhookConfigs == nil {
			httpError(w, http.StatusNotFound, "webhooks not enabled")
			return
		}
		id := r.PathValue("id")
		if id == "" {
			httpError(w, http.StatusBadRequest, "webhook id required")
			return
		}
		// Resolved without a tenant context — the webhook IS the tenant
		// selector. Collapse not-found and provider-mismatch into the
		// same 401 as a bad token so the id space isn't probeable.
		cfg, err := s.webhookConfigs.Get(r.Context(), id)
		if err != nil && !errors.Is(err, webhooks.ErrNotFound) {
			// An outage says nothing about the id — answering it as one
			// would turn every delivery away for good.
			s.webhookConfigUnavailable(w, provider, err)
			return
		}
		if err != nil || cfg.Provider != provider {
			httpError(w, http.StatusUnauthorized, "invalid webhook")
			return
		}
		// Authentication comes before every admission step below: the
		// rate bucket and the monthly quota are the tenant's, and a step
		// that charges them before the sender is proven lets anyone
		// holding the hook's URL spend them.
		//
		// SignModeHMAC providers (GitHub, Forgejo, optionally generic)
		// authenticate the BODY, not a header: the bounded body is read
		// and verified here, then restored for the provider handler,
		// which verifies it again — a handler never trusts that it was
		// reached through this middleware.
		if cfg.SignMode == webhooks.SignModeHMAC {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes))
			if err != nil {
				httpError(w, http.StatusBadRequest, "read body: %v", err)
				return
			}
			if !webhooks.VerifyHMACSignature(s.sealer, cfg.ID, cfg.HMACSecretSealed, body, webhookHMACSignature(provider, r)) {
				if s.logger != nil {
					s.logger.Warn("webhooks: %s bad HMAC for %s from %s", provider, cfg.ID, s.clientIP(r))
				}
				httpError(w, http.StatusUnauthorized, "invalid signature")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		} else if !webhooks.VerifyToken(extractWebhookToken(r, provider), cfg.TokenHash) {
			if s.logger != nil {
				s.logger.Warn("webhooks: bad token for %s from %s", cfg.ID, s.clientIP(r))
			}
			httpError(w, http.StatusUnauthorized, "invalid webhook token")
			return
		}
		if !s.webhookLive(w, r, cfg) || !s.webhookRateAllowed(w, cfg) || !s.webhookQuotaAllowed(w, r, cfg) {
			return
		}
		team, ok := s.webhookTenantActive(w, r, cfg)
		if !ok {
			return
		}
		next.ServeHTTP(w, s.webhookActing(r, cfg, team))
	})
}

// webhookConfigUnavailable answers a delivery whose webhook config could not
// be read: 503 with a fixed text, the cause in the log. An outage does not
// depend on the id, so the answer reveals nothing about which ids exist.
func (s *Server) webhookConfigUnavailable(w http.ResponseWriter, provider webhooks.Provider, err error) {
	if s.logger != nil {
		s.logger.Warn("webhooks: %s delivery: the webhook store is unavailable: %s", provider, boundedText(err.Error(), maxIngressErrorText))
	}
	httpError(w, http.StatusServiceUnavailable, "the webhook store is unavailable; retry")
}

// The admission of an AUTHENTICATED delivery, one helper per step so every
// inbound lane — the provider routes behind webhookAuth and the connector
// ingress lane — applies the same step the same way, each in its own order.
// Every helper answers the refusal itself and reports whether the delivery
// may go on; none may run before the sender is proven: the rate bucket and
// the monthly quota are the tenant's, and a step that charges them before
// the proof lets anyone holding the hook's URL spend them.

// webhookLive refuses a webhook that is disabled, or whose provisioning
// epoch is retired or pending.
func (s *Server) webhookLive(w http.ResponseWriter, r *http.Request, cfg webhooks.Config) bool {
	if !cfg.Enabled {
		httpError(w, http.StatusGone, "webhook disabled")
		return false
	}
	if cfg.ProvisionIntegrationID != "" {
		if s.forgeIntegrations == nil {
			httpError(w, http.StatusServiceUnavailable, "webhook activation store unavailable")
			return false
		}
		ri, err := s.forgeIntegrations.Get(r.Context(), cfg.ProvisionIntegrationID)
		if errors.Is(err, forge.ErrIntegrationNotFound) || err == nil && (ri.TenantID != cfg.TenantID || ri.WebhookID != cfg.ID) {
			httpError(w, http.StatusGone, "webhook epoch retired or pending")
			return false
		}
		if err != nil {
			httpError(w, http.StatusServiceUnavailable, "webhook activation unavailable")
			return false
		}
	}
	return true
}

// webhookRateAllowed applies the per-webhook rate bucket.
func (s *Server) webhookRateAllowed(w http.ResponseWriter, cfg webhooks.Config) bool {
	if s.authLimiter == nil {
		return true
	}
	bucket := authBucketCfg{rate: cfg.RateLimit.Rate, burst: cfg.RateLimit.Burst}
	if bucket.rate <= 0 || bucket.burst <= 0 {
		bucket = authBucketCfg{rate: defaultWebhookRate.Rate, burst: defaultWebhookRate.Burst}
	}
	if ok, retry := s.authLimiter.allow("wh:"+cfg.ID, bucket); !ok {
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.WebhookThrottledTotal.WithLabelValues(string(cfg.Provider), metrics.WebhookThrottledRateLimited).Inc()
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		httpError(w, http.StatusTooManyRequests, "rate limited")
		return false
	}
	return true
}

// webhookQuotaAllowed charges the per-org monthly quota (and the optional
// per-webhook monthly cap): the one step that WRITES, so a lane that must
// write nothing on a refusal runs it last.
func (s *Server) webhookQuotaAllowed(w http.ResponseWriter, r *http.Request, cfg webhooks.Config) bool {
	_, ok := s.chargeWebhookQuota(w, r, cfg)
	return ok
}

// chargeWebhookQuota is webhookQuotaAllowed for a lane that may not keep the
// unit it charged: release gives it back (a write that then fails, a
// delivery that meets the row a concurrent twin wrote).
func (s *Server) chargeWebhookQuota(w http.ResponseWriter, r *http.Request, cfg webhooks.Config) (release func(), ok bool) {
	release = func() {}
	if s.webhookCounter == nil {
		return release, true
	}
	lim := webhooks.Limits{
		PerOrgMonthly:     defaultOrgMonthlyWebhookCalls,
		PerWebhookMonthly: cfg.MonthlyCallLimit,
	}
	now := time.Now().UTC()
	allowed, qerr := s.webhookCounter.Allow(r.Context(), cfg.TenantID, cfg.ID, now, lim)
	if qerr != nil {
		httpError(w, http.StatusInternalServerError, "quota check failed")
		return release, false
	}
	if !allowed {
		if s.cfg.Metrics != nil {
			s.cfg.Metrics.WebhookThrottledTotal.WithLabelValues(string(cfg.Provider), metrics.WebhookThrottledQuotaExceeded).Inc()
		}
		httpError(w, http.StatusTooManyRequests, "monthly call quota exceeded")
		return release, false
	}
	return func() {
		if err := s.webhookCounter.Release(r.Context(), cfg.TenantID, cfg.ID, now, lim); err != nil && s.logger != nil {
			s.logger.Warn("webhooks: release the monthly quota unit of %s: %v", cfg.ID, err)
		}
	}, true
}

// webhookTenantActive refuses a delivery for a tenant that no longer exists
// or may not launch, and returns the loaded Team so the launch gate
// downstream reuses it instead of re-fetching (nil without an auth store).
func (s *Server) webhookTenantActive(w http.ResponseWriter, r *http.Request, cfg webhooks.Config) (*identity.Team, bool) {
	st := s.authStore()
	if st == nil {
		return nil, true
	}
	t, terr := st.GetTeam(r.Context(), cfg.TenantID)
	switch {
	case errors.Is(terr, identity.ErrNotFound):
		// A tenant that no longer exists is not "no constraint to
		// check" — it is a webhook outliving its team. Falling
		// through left a signed delivery launching runs nobody owns
		// and skipping the team-keyed half of the launch gate, which
		// is worse than the suspension this branch guards.
		httpError(w, http.StatusForbidden, "tenant no longer exists")
		return nil, false
	case terr != nil:
		// A degraded identity read must not open the door either;
		// the caller is a machine that will redeliver.
		httpError(w, http.StatusServiceUnavailable, "tenant lookup unavailable")
		return nil, false
	case !t.CanLaunch():
		httpError(w, http.StatusForbidden, "org suspended")
		return nil, false
	}
	return &t, true
}

// webhookActing stamps the tenant identity (the synthetic webhook actor),
// the config and the loaded team on the request, and records the webhook's
// last use.
func (s *Server) webhookActing(r *http.Request, cfg webhooks.Config, team *identity.Team) *http.Request {
	actor := webhookActor(cfg.ID)
	ctx := auth.WithIdentity(r.Context(), auth.Identity{
		UserID: actor,
		TeamID: cfg.TenantID,
		Role:   identity.RoleMember,
		Kind:   auth.KindWebhook,
	})
	ctx = store.WithIdentity(ctx, cfg.TenantID, actor)
	ctx = context.WithValue(ctx, webhookCtxKey{}, cfg)
	if team != nil {
		ctx = withTeam(ctx, *team)
	}
	s.markWebhookUsed(cfg)
	return r.WithContext(ctx)
}

// markWebhookUsed records the webhook's last use. last_used_at is
// observability, not on the inbound critical path — written detached
// (bounded, survives request cancel) so the handler isn't serialised behind
// a Mongo round-trip.
func (s *Server) markWebhookUsed(cfg webhooks.Config) {
	actor := webhookActor(cfg.ID)
	id, now := cfg.ID, time.Now().UTC()
	s.goSafe("webhook-markused", func() {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.webhookConfigs.MarkUsed(store.WithIdentity(bg, cfg.TenantID, actor), id, now)
	})
}
