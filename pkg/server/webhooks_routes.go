package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Reasonable defaults for a new webhook (the operator can override).
var defaultWebhookRate = webhooks.Rate{Rate: 1.0, Burst: 10} // 1 req/s sustained, burst 10

// defaultOrgMonthlyWebhookCalls caps accepted inbound deliveries per org
// per month unless a tighter per-webhook MonthlyCallLimit is set.
const defaultOrgMonthlyWebhookCalls = 10000

// registerWebhookRoutes wires the per-org webhook-token CRUD. Inbound
// delivery routes are registered per provider (registerGitLabWebhookRoute).
func (s *Server) registerWebhookRoutes() {
	if s.authLimiter == nil {
		s.authLimiter = newAuthRateLimiter()
	}
	s.mux.Handle("GET /api/teams/{id}/webhooks", s.requireAuth(http.HandlerFunc(s.handleListWebhooks)))
	s.mux.Handle("POST /api/teams/{id}/webhooks", s.requireAuth(http.HandlerFunc(s.handleCreateWebhook)))
	s.mux.Handle("GET /api/teams/{id}/webhooks/{webhook_id}", s.requireAuth(http.HandlerFunc(s.handleGetWebhook)))
	s.mux.Handle("PATCH /api/teams/{id}/webhooks/{webhook_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateWebhook)))
	s.mux.Handle("DELETE /api/teams/{id}/webhooks/{webhook_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteWebhook)))
	s.mux.Handle("POST /api/teams/{id}/webhooks/{webhook_id}/rotate", s.requireAuth(http.HandlerFunc(s.handleRotateWebhook)))
	s.mux.Handle("GET /api/teams/{id}/webhooks/{webhook_id}/deliveries", s.requireAuth(http.HandlerFunc(s.handleListWebhookDeliveries)))
}

type webhookConfigReq struct {
	Name               *string           `json:"name,omitempty"`
	Provider           *string           `json:"provider,omitempty"`
	SignMode           *string           `json:"sign_mode,omitempty"`
	Enabled            *bool             `json:"enabled,omitempty"`
	BotIDs             []string          `json:"bot_ids,omitempty"`
	WildcardBots       *bool             `json:"wildcard_bots,omitempty"`
	DefaultBotID       *string           `json:"default_bot_id,omitempty"`
	ProjectAllowlist   []string          `json:"project_allowlist,omitempty"`
	EventAllowlist     []string          `json:"event_allowlist,omitempty"`
	AuthorAllowlist    []string          `json:"author_allowlist,omitempty"`
	RateLimit          *webhooks.Rate    `json:"rate_limit,omitempty"`
	MonthlyCallLimit   *int              `json:"monthly_call_limit,omitempty"`
	LaunchVars         map[string]string `json:"launch_vars,omitempty"`
	KeyOverrides       map[string]string `json:"key_overrides,omitempty"`
	SecretOverrides    map[string]string `json:"secret_overrides,omitempty"`
	AuthorizedRepliers []string          `json:"authorized_repliers,omitempty"`
	MinReplierRole     *string           `json:"min_replier_role,omitempty"`
	ForgeBaseURL       *string           `json:"forge_base_url,omitempty"`
}

// supportedProviders is the closed enum the create endpoint accepts.
// Listing it once (vs. an ad-hoc switch) lets the UI and API docs
// reflect the same source of truth.
var supportedProviders = map[webhooks.Provider]bool{
	webhooks.ProviderGitLab:  true,
	webhooks.ProviderGitHub:  true,
	webhooks.ProviderForgejo: true,
	webhooks.ProviderGeneric: true,
}

// defaultSignMode picks the right SignatureMode for a provider when the
// caller leaves it unset. GitHub + Forgejo only ever sign the body;
// GitLab + the generic webhook stick to header-token auth unless the
// caller explicitly opts in to HMAC (generic-only). Centralised here so
// the create handler stays readable.
func defaultSignMode(provider webhooks.Provider, req *string) (webhooks.SignatureMode, error) {
	if req != nil && *req != "" {
		mode := webhooks.SignatureMode(*req)
		switch mode {
		case webhooks.SignModeToken, webhooks.SignModeHMAC:
		default:
			return "", fmt.Errorf("unsupported sign_mode %q (token|hmac)", mode)
		}
		// Only the generic provider lets the operator pick; the
		// forge-specific providers carry a fixed sign mode (otherwise
		// we'd send GitLab's secret-token down the HMAC code path).
		switch provider {
		case webhooks.ProviderGitHub, webhooks.ProviderForgejo:
			if mode != webhooks.SignModeHMAC {
				return "", fmt.Errorf("sign_mode for %s is always hmac", provider)
			}
		case webhooks.ProviderGitLab:
			if mode != webhooks.SignModeToken {
				return "", fmt.Errorf("sign_mode for gitlab is always token")
			}
		case webhooks.ProviderGeneric:
			// either mode is fine
		}
		return mode, nil
	}
	switch provider {
	case webhooks.ProviderGitHub, webhooks.ProviderForgejo:
		return webhooks.SignModeHMAC, nil
	default:
		return webhooks.SignModeToken, nil
	}
}

type webhookWithToken struct {
	Config webhooks.Config `json:"config"`
	Token  string          `json:"token"`
}

// normalizeBotScope validates the bot-scope inputs: a wildcard scope
// must be explicit (wildcard_bots:true → BotIDs forced to ["*"]); a
// non-wildcard scope must list ≥1 non-"*" bot.
func normalizeBotScope(botIDs []string, wildcard bool) ([]string, bool, error) {
	if wildcard {
		return []string{"*"}, true, nil
	}
	if len(botIDs) == 0 {
		return nil, false, errors.New("bot_ids required (or set wildcard_bots:true)")
	}
	for _, b := range botIDs {
		if strings.TrimSpace(b) == "" {
			return nil, false, errors.New("empty bot id")
		}
		if b == "*" {
			return nil, false, errors.New(`a bare "*" bot scope requires wildcard_bots:true`)
		}
	}
	return botIDs, false, nil
}

// webhookForTenant fetches a config and asserts it belongs to teamID
// (defence in depth on top of the role gate). Returns ok=false having
// already written the response.
func (s *Server) webhookForTenant(w http.ResponseWriter, r *http.Request, teamID, webhookID string) (webhooks.Config, bool) {
	cfg, err := s.webhookConfigs.Get(r.Context(), webhookID)
	if err != nil || cfg.TenantID != teamID {
		httpError(w, http.StatusNotFound, "webhook not found")
		return webhooks.Config{}, false
	}
	return cfg, true
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	list, err := s.webhookConfigs.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []webhooks.Config{}
	}
	writeJSON(w, struct {
		Webhooks []webhooks.Config `json:"webhooks"`
	}{Webhooks: list})
}

func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	cfg, ok := s.webhookForTenant(w, r, teamID, r.PathValue("webhook_id"))
	if !ok {
		return
	}
	writeJSON(w, cfg)
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req webhookConfigReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == nil || *req.Name == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	provider := webhooks.ProviderGitLab
	if req.Provider != nil && *req.Provider != "" {
		provider = webhooks.Provider(*req.Provider)
	}
	if !supportedProviders[provider] {
		httpError(w, http.StatusBadRequest, "unsupported provider %q (gitlab|github|forgejo|generic)", provider)
		return
	}
	signMode, err := defaultSignMode(provider, req.SignMode)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	wildcard := req.WildcardBots != nil && *req.WildcardBots
	botIDs, wildcard, err := normalizeBotScope(req.BotIDs, wildcard)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	plaintext, hash, last4, fp, err := webhooks.MintToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "mint token: %v", err)
		return
	}
	now := time.Now().UTC()
	rate := defaultWebhookRate
	if req.RateLimit != nil {
		rate = *req.RateLimit
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	cfg := webhooks.Config{
		ID:                 uuid.NewString(),
		TenantID:           teamID,
		Name:               *req.Name,
		Provider:           provider,
		SignMode:           signMode,
		Enabled:            enabled,
		TokenHash:          hash,
		TokenLast4:         last4,
		Fingerprint:        fp,
		BotIDs:             botIDs,
		WildcardBots:       wildcard,
		ProjectAllowlist:   req.ProjectAllowlist,
		EventAllowlist:     req.EventAllowlist,
		AuthorAllowlist:    req.AuthorAllowlist,
		RateLimit:          rate,
		LaunchVars:         req.LaunchVars,
		KeyOverrides:       req.KeyOverrides,
		SecretOverrides:    req.SecretOverrides,
		AuthorizedRepliers: req.AuthorizedRepliers,
		CreatedBy:          id.UserID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if req.DefaultBotID != nil {
		cfg.DefaultBotID = *req.DefaultBotID
	}
	if req.MonthlyCallLimit != nil {
		cfg.MonthlyCallLimit = *req.MonthlyCallLimit
	}
	if req.MinReplierRole != nil {
		cfg.MinReplierRole = *req.MinReplierRole
	}
	if req.ForgeBaseURL != nil {
		normalized, verr := validateForgeBaseURL(*req.ForgeBaseURL)
		if verr != nil {
			httpError(w, http.StatusBadRequest, "%s", verr.Error())
			return
		}
		cfg.ForgeBaseURL = normalized
	}
	// HMAC-mode webhooks need the minted plaintext sealed at rest so a
	// later request can recompute HMAC(body) — the operator pastes the
	// same iwh_ value into the forge's "secret" field.
	if signMode == webhooks.SignModeHMAC {
		sealed, err := s.sealWebhookHMAC(cfg.ID, plaintext)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "seal hmac secret: %v", err)
			return
		}
		cfg.HMACSecretSealed = sealed
	}
	if err := s.validateKeyOverrides(r.Context(), teamID, cfg.KeyOverrides); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.validateSecretOverrides(r.Context(), teamID, cfg.SecretOverrides); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.webhookConfigs.Create(r.Context(), cfg); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if wildcard && s.logger != nil {
		s.logger.Warn("webhooks: org %s created a wildcard-bot webhook %s (by %s)", teamID, cfg.ID, id.UserID)
	}
	s.auditTenant(r, teamID, "webhook.created", "webhook", cfg.ID, map[string]any{"name": cfg.Name, "provider": string(cfg.Provider), "wildcard_bots": cfg.WildcardBots})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, webhookWithToken{Config: cfg, Token: plaintext})
}

// sealWebhookHMAC is the thin wrapper around webhooks.SealHMACSecret —
// it pins the AAD to the webhook ID and surfaces a clean error when
// the server was built without a sealer (a deployment misconfiguration
// — hmac-mode webhooks require Sealer to be wired).
func (s *Server) sealWebhookHMAC(webhookID, plaintext string) ([]byte, error) {
	if s.sealer == nil {
		return nil, errors.New("server: hmac webhook requires sealer (configure Sealer)")
	}
	return webhooks.SealHMACSecret(s.sealer, webhookID, plaintext)
}

// validateForgeBaseURL normalizes a webhook's optional forge pin. Empty is
// allowed (no pin). A non-empty value must be an absolute https URL with a
// host and no userinfo; it is normalized to scheme://host (path/query dropped)
// so the runtime can compare it against the payload's MR-URL host.
func validateForgeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("forge_base_url must be an absolute https URL with a host (e.g. https://gitlab.example.com)")
	}
	return "https://" + u.Host, nil
}

// validateKeyOverrides rejects a webhook key override that references a BYOK
// key the webhook's tenant doesn't own, or whose provider doesn't match. The
// resolver is already tenant-scoped (a foreign key_id silently won't match in
// secrets.Resolve), so this is a fail-fast UX guard, not the security boundary.
func (s *Server) validateKeyOverrides(ctx context.Context, tenantID string, overrides map[string]string) error {
	if len(overrides) == 0 || s.apiKeys == nil {
		return nil
	}
	for prov, keyID := range overrides {
		k, err := s.apiKeys.Get(ctx, keyID)
		if err != nil {
			return fmt.Errorf("key_overrides[%s]: api key %q not found", prov, keyID)
		}
		if k.TenantID != tenantID {
			return fmt.Errorf("key_overrides[%s]: api key %q belongs to another org", prov, keyID)
		}
		if string(k.Provider) != prov {
			return fmt.Errorf("key_overrides[%s]: api key %q is for provider %q", prov, keyID, k.Provider)
		}
	}
	return nil
}

// validateSecretOverrides rejects a webhook secret override that references a
// stored secret the webhook's tenant doesn't own at the org level. Like the
// key-override guard, the resolver is already tenant-scoped (a foreign id
// silently won't bind), so this is a fail-fast UX check.
func (s *Server) validateSecretOverrides(ctx context.Context, tenantID string, overrides map[string]string) error {
	if len(overrides) == 0 || s.genericSecrets == nil {
		return nil
	}
	for name, secretID := range overrides {
		sec, err := s.genericSecrets.Get(ctx, secretID)
		if err != nil {
			return fmt.Errorf("secret_overrides[%s]: stored secret %q not found", name, secretID)
		}
		if sec.ScopeTeamID != tenantID || sec.ScopeUserID != "" {
			return fmt.Errorf("secret_overrides[%s]: secret %q must be an org-scoped secret of this team", name, secretID)
		}
	}
	return nil
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	cfg, ok := s.webhookForTenant(w, r, teamID, r.PathValue("webhook_id"))
	if !ok {
		return
	}
	var req webhookConfigReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		cfg.Name = *req.Name
	}
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.DefaultBotID != nil {
		cfg.DefaultBotID = *req.DefaultBotID
	}
	if req.ProjectAllowlist != nil {
		cfg.ProjectAllowlist = req.ProjectAllowlist
	}
	if req.EventAllowlist != nil {
		cfg.EventAllowlist = req.EventAllowlist
	}
	if req.AuthorAllowlist != nil {
		cfg.AuthorAllowlist = req.AuthorAllowlist
	}
	if req.RateLimit != nil {
		cfg.RateLimit = *req.RateLimit
	}
	if req.MonthlyCallLimit != nil {
		cfg.MonthlyCallLimit = *req.MonthlyCallLimit
	}
	if req.LaunchVars != nil {
		cfg.LaunchVars = req.LaunchVars
	}
	if req.KeyOverrides != nil {
		if err := s.validateKeyOverrides(r.Context(), teamID, req.KeyOverrides); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		cfg.KeyOverrides = req.KeyOverrides
	}
	if req.SecretOverrides != nil {
		if err := s.validateSecretOverrides(r.Context(), teamID, req.SecretOverrides); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		cfg.SecretOverrides = req.SecretOverrides
	}
	if req.AuthorizedRepliers != nil {
		cfg.AuthorizedRepliers = req.AuthorizedRepliers
	}
	if req.MinReplierRole != nil {
		cfg.MinReplierRole = *req.MinReplierRole
	}
	if req.ForgeBaseURL != nil {
		normalized, verr := validateForgeBaseURL(*req.ForgeBaseURL)
		if verr != nil {
			httpError(w, http.StatusBadRequest, "%s", verr.Error())
			return
		}
		cfg.ForgeBaseURL = normalized
	}
	// Re-normalise bot scope only when the caller touched it.
	if req.BotIDs != nil || req.WildcardBots != nil {
		wildcard := cfg.WildcardBots
		if req.WildcardBots != nil {
			wildcard = *req.WildcardBots
		}
		botIDs := cfg.BotIDs
		if req.BotIDs != nil {
			botIDs = req.BotIDs
		}
		newBotIDs, newWildcard, err := normalizeBotScope(botIDs, wildcard)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		cfg.BotIDs, cfg.WildcardBots = newBotIDs, newWildcard
	}
	cfg.UpdatedAt = time.Now().UTC()
	if err := s.webhookConfigs.Update(r.Context(), cfg); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "webhook.updated", "webhook", cfg.ID, map[string]any{"name": cfg.Name, "enabled": cfg.Enabled})
	writeJSON(w, cfg)
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	cfg, ok := s.webhookForTenant(w, r, teamID, r.PathValue("webhook_id"))
	if !ok {
		return
	}
	if cfg.ProvisionedBy != "" {
		httpError(w, http.StatusConflict, "this webhook is managed by a forge integration — disable it from the Integrations tab instead")
		return
	}
	if err := s.webhookConfigs.Delete(r.Context(), r.PathValue("webhook_id")); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "webhook.deleted", "webhook", r.PathValue("webhook_id"), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRotateWebhook(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	cfg, ok := s.webhookForTenant(w, r, teamID, r.PathValue("webhook_id"))
	if !ok {
		return
	}
	if cfg.ProvisionedBy != "" {
		// Rotating a managed webhook's token without re-pushing it to the
		// forge hook would silently brick delivery — re-provision via the
		// Integrations tab instead (it re-mints + UpdateHook atomically).
		httpError(w, http.StatusConflict, "this webhook is managed by a forge integration — rotate it by disabling and re-enabling the bot in the Integrations tab")
		return
	}
	plaintext, hash, last4, fp, err := webhooks.MintToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "mint token: %v", err)
		return
	}
	now := time.Now().UTC()
	cfg.TokenHash, cfg.TokenLast4, cfg.Fingerprint = hash, last4, fp
	cfg.RotatedAt = &now
	cfg.UpdatedAt = now
	// Reseal the HMAC plaintext under the new token so an hmac-mode
	// webhook keeps verifying after rotate. The operator must update
	// the forge's "secret" field with the same new plaintext.
	if cfg.SignMode == webhooks.SignModeHMAC {
		sealed, err := s.sealWebhookHMAC(cfg.ID, plaintext)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "seal hmac secret: %v", err)
			return
		}
		cfg.HMACSecretSealed = sealed
	}
	if err := s.webhookConfigs.Update(r.Context(), cfg); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "webhook.rotated", "webhook", cfg.ID, map[string]any{"name": cfg.Name})
	writeJSON(w, webhookWithToken{Config: cfg, Token: plaintext})
}

func (s *Server) handleListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	if _, ok := s.webhookForTenant(w, r, teamID, r.PathValue("webhook_id")); !ok {
		return
	}
	if s.webhookDeliveries == nil {
		writeJSON(w, struct {
			Deliveries []webhooks.Delivery `json:"deliveries"`
		}{Deliveries: []webhooks.Delivery{}})
		return
	}
	list, err := s.webhookDeliveries.ListByWebhook(r.Context(), teamID, r.PathValue("webhook_id"), 100)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []webhooks.Delivery{}
	}
	writeJSON(w, struct {
		Deliveries []webhooks.Delivery `json:"deliveries"`
	}{Deliveries: list})
}
