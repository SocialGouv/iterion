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
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/secrets"
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
	s.mux.Handle("POST /api/webhooks/deliveries/{id}/retry", s.requireSuperAdmin(s.acknowledgeWebhookLaunch(http.HandlerFunc(s.handleRetryWebhookDelivery))))
	if s.authLimiter == nil {
		s.authLimiter = newAuthRateLimiter()
	}
	s.mux.Handle("GET /api/teams/{id}/webhooks", s.requireAuth(http.HandlerFunc(s.handleListWebhooks)))
	// The create and the rotate mint a self-authenticating token that
	// outlives the credential it is minted from: they go through the
	// derivation chokepoint (auth.MintTeamToken), so the access token of a
	// revoked session mints none (401) and a personal access token never
	// does (403).
	s.mux.Handle("POST /api/teams/{id}/webhooks", s.requireAuth(s.gateDerivation(auth.MintTeamToken, http.HandlerFunc(s.handleCreateWebhook))))
	s.mux.Handle("GET /api/teams/{id}/webhooks/{webhook_id}", s.requireAuth(http.HandlerFunc(s.handleGetWebhook)))
	s.mux.Handle("PATCH /api/teams/{id}/webhooks/{webhook_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateWebhook)))
	s.mux.Handle("DELETE /api/teams/{id}/webhooks/{webhook_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteWebhook)))
	s.mux.Handle("POST /api/teams/{id}/webhooks/{webhook_id}/rotate", s.requireAuth(s.gateDerivation(auth.MintTeamToken, http.HandlerFunc(s.handleRotateWebhook))))
	s.mux.Handle("GET /api/teams/{id}/webhooks/{webhook_id}/deliveries", s.requireAuth(http.HandlerFunc(s.handleListWebhookDeliveries)))
}

type webhookConfigReq struct {
	// No `routing` field, deliberately (ADR-129): a MANAGED config's
	// routing block is provisioner-owned — the forge integration record is
	// the durable carrier, and Provision rebuilds this document from it on
	// every enable, so a PATCH-written block would be a silent
	// write-here-lost-at-next-enable. The write surface is the repo-bots
	// API (forge_provisioning_routes.go). An UNMANAGED config's block is a
	// separate decision nobody has asked for yet.
	Name                *string  `json:"name,omitempty"`
	Provider            *string  `json:"provider,omitempty"`
	SignMode            *string  `json:"sign_mode,omitempty"`
	Enabled             *bool    `json:"enabled,omitempty"`
	BotIDs              []string `json:"bot_ids,omitempty"`
	WildcardBots        *bool    `json:"wildcard_bots,omitempty"`
	DefaultBotID        *string  `json:"default_bot_id,omitempty"`
	ProjectAllowlist    []string `json:"project_allowlist,omitempty"`
	EventAllowlist      []string `json:"event_allowlist,omitempty"`
	AuthorAllowlist     []string `json:"author_allowlist,omitempty"`
	LabelAllowlist      []string `json:"label_allowlist,omitempty"`
	HoldLabels          []string `json:"hold_labels,omitempty"`
	ReviewOnSync        *bool    `json:"review_on_sync,omitempty"`
	ReviewRequestLogins []string `json:"review_request_logins,omitempty"`
	// ReviewOnSyncPinned clears (false) or restates (true) the provenance
	// pin an explicit review_on_sync set leaves behind; absent = derived
	// from whether review_on_sync itself is being set.
	ReviewOnSyncPinned  *bool             `json:"review_on_sync_pinned,omitempty"`
	Overlap             *string           `json:"overlap,omitempty"`
	AutoImplementOnOpen *bool             `json:"auto_implement_on_open,omitempty"`
	BranchImproveAsPR   *bool             `json:"branch_improve_as_pr,omitempty"`
	RateLimit           *webhooks.Rate    `json:"rate_limit,omitempty"`
	MonthlyCallLimit    *int              `json:"monthly_call_limit,omitempty"`
	LaunchVars          map[string]string `json:"launch_vars,omitempty"`
	KeyOverrides        map[string]string `json:"key_overrides,omitempty"`
	SecretOverrides     map[string]string `json:"secret_overrides,omitempty"`
	AuthorizedRepliers  []string          `json:"authorized_repliers,omitempty"`
	MinReplierRole      *string           `json:"min_replier_role,omitempty"`
	ForgeBaseURL        *string           `json:"forge_base_url,omitempty"`
	// ConnectorConnectionID binds the webhook to a pkg/connection record:
	// set, the inbound handler is the connector lane (#2761). An explicit
	// empty string clears the binding.
	ConnectorConnectionID *string `json:"connector_connection_id,omitempty"`
	// ForkLane opts the config into the fork review lane at CREATION
	// (ADR-162 decision 1). Github-only, checked at creation with a typed
	// refusal per other provider; IMMUTABLE afterwards — the PATCH arm
	// below refuses any change, so a value present on the request is
	// honoured only when it restates the stored one.
	ForkLane *bool `json:"fork_lane,omitempty"`
	// ForkLanePool names the runner pool every run of the fork lane executes
	// on (ADR-162 Q3, decision 10). Required at creation with fork_lane,
	// refused without it, IMMUTABLE afterwards on the same terms as
	// fork_lane: the PATCH arm honours it only as a restatement.
	ForkLanePool *string `json:"fork_lane_pool,omitempty"`
}

// forkLanePoolCreationRefusal is the creation's word on fork_lane_pool: a
// fork-lane config names the pool its runs execute on, an ordinary config
// names none, and the name is a pool name. "" admits. The pool's registry
// state, and whether a team other than this one holds it, are judged where
// the run is published (cloudpublisher's fork-lane pool rule), because both
// can move after the config is created.
func forkLanePoolCreationRefusal(forkLane bool, pool string) string {
	switch {
	case forkLane && pool == "":
		return "fork_lane requires fork_lane_pool: the runner pool every run of the lane executes on — a pool whose runners resolve the kubernetes sandbox for every run (ADR-162 decision 10). It is set at creation only and immutable afterwards"
	case !forkLane && pool != "":
		return "fork_lane_pool names the runner pool of a fork-lane config, and this config is not one (fork_lane is not set): an ordinary config's runs execute on their team's own mapping"
	case pool != "" && !queue.ValidPoolName(pool):
		return fmt.Sprintf("fork_lane_pool %q is not a runner pool name (1–31 chars [a-z0-9-], alphanumeric at both ends — the registry's grammar)", pool)
	}
	return ""
}

// forkLaneProviderRefusal is the typed creation refusal for a fork-lane
// config on a provider whose lane does not exist, one arm per provider
// because each names the witness it lacks — the row an operator can act
// on rather than a generic "not supported".
func forkLaneProviderRefusal(provider webhooks.Provider) error {
	switch provider {
	case webhooks.ProviderGitLab:
		return errors.New("fork_lane is github-only: GitLab's merge-request head ref (refs/merge-requests/<iid>/head) is unwired — the seam is named in docs/webhooks.md, nothing launches on it")
	case webhooks.ProviderForgejo:
		return errors.New("fork_lane is github-only: Forgejo's head-repository proof, author id and pull-request ref have no witness — the admission would take the payload's word for all three")
	case webhooks.ProviderGeneric:
		return errors.New("fork_lane is github-only: the generic webhook has no pull request to admit")
	default:
		return fmt.Errorf("fork_lane is github-only (provider %q)", provider)
	}
}

// overlapOrEmpty resolves an optional overlap policy. Empty is meaningful
// here — a webhook with no policy launches every delivery, which is the
// historical behaviour every existing webhook relies on.
func overlapOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
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

// webhookWithToken is the create and rotate answer: the row and the one
// plaintext of its token. PinsUnreconciled names, on a create, the record
// ids its landed pins name that the route could not re-check after the row
// was stored (see reconcileLandedPins) — a receipt the client acts on by
// re-reading those records and clearing a pin naming a dead one; absent
// when every pin was vouched for.
type webhookWithToken struct {
	Config           webhooks.Config `json:"config"`
	Token            string          `json:"token"`
	PinsUnreconciled []string        `json:"pins_unreconciled,omitempty"`
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

// readWebhookForTenant is the foreignness clause itself, in ONE place: a
// config addressed by a CALLER-supplied id serves a team only when it is
// stamped with that team — the gate above only proves standing in the team
// named in the path, so without this an admin of team A would read, patch,
// delete and rotate team B's webhook by guessing its id.
//
// A foreign config is indistinguishable from an absent one, which is what
// keeps ids from being enumerable across tenants: both answer
// webhooks.ErrNotFound. A read that FAILED returns the store's own error, so
// a caller that must tell a transient fault from a refusal can
// (handleCredentialPreview answers that one 503).
func (s *Server) readWebhookForTenant(ctx context.Context, teamID, webhookID string) (webhooks.Config, error) {
	cfg, err := s.webhookConfigs.Get(ctx, webhookID)
	if err != nil {
		return webhooks.Config{}, err
	}
	if cfg.TenantID != teamID {
		return webhooks.Config{}, webhooks.ErrNotFound
	}
	return cfg, nil
}

// webhookForTenant is the by-id CRUD's rendering of that clause: one 404 for
// an absent id, a foreign config and an unreadable store alike. Returns
// ok=false having already written the response.
func (s *Server) webhookForTenant(w http.ResponseWriter, r *http.Request, teamID, webhookID string) (webhooks.Config, bool) {
	cfg, err := s.readWebhookForTenant(r.Context(), teamID, webhookID)
	if err != nil {
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
	// The fork review lane is github-only (ADR-162 decision 1), checked
	// HERE at creation rather than at delivery: a config that could be
	// created on another provider would sit enabled with no lane behind
	// it, silently swallowing deliveries. Typed per provider — the refusal
	// names the witness the provider lacks, which is the fact an operator
	// needs before asking for that lane.
	if req.ForkLane != nil && *req.ForkLane && provider != webhooks.ProviderGitHub {
		httpError(w, http.StatusBadRequest, "%s", forkLaneProviderRefusal(provider).Error())
		return
	}
	// The lane's pool is set HERE or never (ADR-162 Q3): a fork-lane config
	// created without one could never launch — the field is immutable — and
	// would sit enabled swallowing every gesture as a filtered row.
	forkLanePool := ""
	if req.ForkLanePool != nil {
		forkLanePool = strings.TrimSpace(*req.ForkLanePool)
	}
	if reason := forkLanePoolCreationRefusal(req.ForkLane != nil && *req.ForkLane, forkLanePool); reason != "" {
		httpError(w, http.StatusBadRequest, "%s", reason)
		return
	}
	wildcard := req.WildcardBots != nil && *req.WildcardBots
	botIDs, wildcard, err := normalizeBotScope(req.BotIDs, wildcard)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	plaintext, hash, last4, fp, err := webhooks.MintToken(r.Context())
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
		ID:                  uuid.NewString(),
		TenantID:            teamID,
		Name:                *req.Name,
		Provider:            provider,
		SignMode:            signMode,
		Enabled:             enabled,
		TokenHash:           hash,
		TokenLast4:          last4,
		Fingerprint:         fp,
		BotIDs:              botIDs,
		WildcardBots:        wildcard,
		ProjectAllowlist:    req.ProjectAllowlist,
		EventAllowlist:      req.EventAllowlist,
		AuthorAllowlist:     req.AuthorAllowlist,
		LabelAllowlist:      req.LabelAllowlist,
		HoldLabels:          req.HoldLabels,
		ReviewOnSync:        req.ReviewOnSync != nil && *req.ReviewOnSync,
		ReviewRequestLogins: req.ReviewRequestLogins,
		ReviewOnSyncPinned:  req.ReviewOnSync != nil, // an explicit set at create is a decision too
		Overlap:             overlapOrEmpty(req.Overlap),
		AutoImplementOnOpen: req.AutoImplementOnOpen != nil && *req.AutoImplementOnOpen,
		RateLimit:           rate,
		RateLimitPinned:     req.RateLimit != nil, // same rule as ReviewOnSyncPinned
		LaunchVars:          req.LaunchVars,
		KeyOverrides:        req.KeyOverrides,
		SecretOverrides:     req.SecretOverrides,
		AuthorizedRepliers:  req.AuthorizedRepliers,
		CreatedBy:           id.UserID,
		CreatedAt:           now,
		UpdatedAt:           now,
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
	if req.ConnectorConnectionID != nil && *req.ConnectorConnectionID != "" {
		if err := s.validateConnectorBinding(r.Context(), teamID, provider, *req.ConnectorConnectionID); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		cfg.ConnectorConnectionID = *req.ConnectorConnectionID
	}
	if req.ForkLane != nil {
		cfg.ForkLane = *req.ForkLane
	}
	cfg.ForkLanePool = forkLanePool
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
	if err := s.validateKeyOverrides(teamTenantCtx(r.Context(), teamID), teamID, webhookActor(cfg.ID), cfg.KeyOverrides); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.validateSecretOverrides(teamTenantCtx(r.Context(), teamID), teamID, cfg.SecretOverrides); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.webhookConfigs.Create(r.Context(), cfg); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// The pins just landed; close the born-mid-delete window before the
	// answer — reconcileLandedPins sweeps a record revoked between this
	// write's validation and its landing, the row the delete's sweep
	// never listed (see credential_pin_sweep.go). The row is stored and
	// the token below is its only copy, so a failed re-check is a receipt
	// on the 201 (pins_unreconciled, cause in the log), never a 5xx that
	// would lose the token and send the retry minting a second webhook.
	guardCtx, guardCancel := pinGuardCtx(r)
	defer guardCancel()
	pins := s.reconcileLandedPins(guardCtx, teamID, cfg.KeyOverrides, cfg.SecretOverrides)
	meta := map[string]any{"name": cfg.Name, "provider": string(cfg.Provider), "wildcard_bots": cfg.WildcardBots}
	if len(pins.Cleared) > 0 {
		meta["pins_cleared"] = pins.Cleared
	}
	if pins.Err != nil {
		meta["pins_unreconciled"] = pins.Unreconciled
		s.logUnreconciledPins("webhook", cfg.ID, pins)
	}
	if wildcard && s.logger != nil {
		s.logger.Warn("webhooks: org %s created a wildcard-bot webhook %s (by %s)", teamID, cfg.ID, id.UserID)
	}
	s.auditTenant(r, teamID, "webhook.created", "webhook", cfg.ID, meta)
	// The answer must not resell the swept pins — the honesty the delete's
	// 204 owes: answer with the row as the store now holds it.
	pins.strip(cfg.KeyOverrides)
	pins.strip(cfg.SecretOverrides)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, webhookWithToken{Config: cfg, Token: plaintext, PinsUnreconciled: pins.Unreconciled})
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

// validateCredentialPinKeys rejects a pin name a Mongo path cannot address.
// Both stores hold the pins as one subdocument and the sweep's $unset
// addresses each entry as "<field>.<name>", so a name holding a dot reads
// as a nested path — the $unset matches nothing, the clear reports nothing
// removed, and the dead pin stands while the delete answers 204 — and a $
// names an operator or an unstashable field. It runs inside the validators,
// before their store-nil early return: addressability holds whether or not
// the record store is wired, and both machine lanes' routes pass through
// this one guard. New names are therefore always addressable; the stores'
// clears keep a rewrite arm for names stored before this guard existed
// (storekit.PinNameAddressable is the one definition of the shape).
func validateCredentialPinKeys(field string, pins map[string]string) error {
	for name := range pins {
		if name == "" {
			return fmt.Errorf("%s: a pin name is required", field)
		}
		if !storekit.PinNameAddressable(name) {
			return fmt.Errorf("%s[%q]: a pin name must not hold '.' or '$' — the pin sweep addresses each entry as a Mongo path", field, name)
		}
	}
	return nil
}

// validateKeyOverrides rejects a machine lane's key override that references
// a BYOK key the caller's tenant doesn't own, whose provider doesn't match,
// or that the lane's own actor could never spend. The resolver is already
// tenant-scoped (a foreign key_id silently won't match in secrets.Resolve),
// so the cross-org arm is a fail-fast UX guard, not the security boundary;
// the spendability arm is the opposite — it is the ONLY place that arm can be
// enforced, because an unspendable pin produces no refusal downstream at all.
// Both machine lanes that carry pins share it: the webhook routes and the
// trigger subscription routes.
//
// actor is the identity the launch will resolve credentials under
// (webhookActor / triggerSpineActor for the machine lanes). It fails CLOSED:
// an empty actor matches no key's scope, so an unknown lane is treated as a
// machine lane rather than skipping the arm — a credential guard with an
// unused opt-out is a seam for the next caller's false negative.
func (s *Server) validateKeyOverrides(ctx context.Context, tenantID, actor string, overrides map[string]string) error {
	if err := validateCredentialPinKeys("key_overrides", overrides); err != nil {
		return err
	}
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
		// A gateway key pins like any key of the team: the walk spends it,
		// and the launch keeps such a pin exclusive (ADR-131 §6 — a pinned
		// gateway key the run cannot seal refuses the launch by name,
		// cloudpublisher's gatewayPinRefusal), so nothing here judges the
		// provider.
		//
		// The one key a launch cannot spend is refused here: this lane fires
		// under a synthetic actor no key is scoped to, so secrets.Resolve
		// never even lists a user-scoped key as a candidate — a vendor pin is
		// dropped and the run seals the team's default instead. Resolve
		// cannot report it (an invisible key never reaches onWithheld, which
		// answers for the bot and repository audiences only), so the write
		// refuses it, and the operator's explicit choice is never replaced in
		// silence. The secret twin below has always refused the same shape.
		if !secrets.SpendableBy(k, tenantID, actor) {
			return fmt.Errorf("key_overrides[%s]: api key %q is not spendable by this lane — it launches as %q, which sees only this team's org-scoped keys, so the pin would be dropped at launch; pin an org-scoped key of this team or clear the pin", prov, keyID, actor)
		}
	}
	return nil
}

// validateSecretOverrides rejects a webhook secret override that references a
// stored secret the webhook's tenant doesn't own at the org level. Like the
// key-override guard, the resolver is already tenant-scoped (a foreign id
// silently won't bind), so this is a fail-fast UX check.
func (s *Server) validateSecretOverrides(ctx context.Context, tenantID string, overrides map[string]string) error {
	if err := validateCredentialPinKeys("secret_overrides", overrides); err != nil {
		return err
	}
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
	// The lane KIND is immutable on an existing config (ADR-162 decision
	// 1). A flip would re-read every parked debounce target of the other
	// kind through the disjointness gate — silently terminally, since the
	// stored provenance cannot change with the config — and the
	// per-author budget's rows were written under the kind the config
	// carried. A restatement of the stored value is a no-op, so the field
	// stays round-trippable for a client that echoes the row back.
	if req.ForkLane != nil && *req.ForkLane != cfg.ForkLane {
		httpError(w, http.StatusConflict,
			"fork_lane is immutable on an existing webhook: flipping the lane kind would re-read its parked debounce targets as the other kind and re-interpret the per-author budget's rows — delete and recreate the webhook instead")
		return
	}
	// The lane's pool is immutable on the same terms: moving it would split
	// one lane's runs across two pools, the in-flight ones resuming on the
	// pool their documents froze and the next ones launching on the other.
	// A restatement is a no-op, as for fork_lane.
	if req.ForkLanePool != nil && strings.TrimSpace(*req.ForkLanePool) != cfg.ForkLanePool {
		httpError(w, http.StatusConflict,
			"fork_lane_pool is immutable on an existing webhook: the lane's runs are stamped with the pool at launch and every resume follows that stamp, so a new pool would split the lane across two — delete and recreate the webhook instead")
		return
	}
	// An explicit overlap clear IS a deliberate gesture, and it must be a
	// READABLE one before anything downstream decides on the raw request.
	// The normalization — "" → "allow", enforcement-identical
	// (webhooks.Config documents empty = allow; with MaxConcurrent 0
	// EvaluateOverlap is unlimited) — serves two masters, both load-bearing.
	// The provision adopt's config-wins rule needs a readable value to win
	// with: stored "", a scalar's empty reads as never-set, and the next
	// silent re-provision would resurrect the integration's stale value over
	// the operator's clear. And it must happen AHEAD of
	// webhookPatchExpandsSurface precisely so the clear trips the approval
	// gate exactly like a literal "allow": widening skip/supersede → allow
	// IS an expansion of the repo's automated surface, and an unreadable ""
	// would walk straight past the predicate.
	if req.Overlap != nil {
		v := strings.TrimSpace(*req.Overlap)
		if v == "" {
			v = schedgate.OverlapAllow
		}
		*req.Overlap = v
	}
	// Org approval-gate parity: a MANAGED (forge-provisioned) config is the
	// runtime face of a repo integration, and it carries the same automation
	// switches the provisioning gate parks (bots, hold labels, allowlist,
	// zero-touch lanes, overlap). Letting a team admin PATCH those here
	// would bypass Org.RequireProvisionApproval entirely — so an EXPANDING
	// patch on a managed config is refused toward the integrations API,
	// whose approval flow is the governed path. Tightenings and neutral
	// fields (launch vars, rate limits, names) stay direct.
	if strings.HasPrefix(cfg.ProvisionedBy, "forge:") {
		// The CRUD writes no pool block, but a managed config CARRIES the
		// integration's: an expanding patch (more bots, a wildcard) widens
		// who draws that pool, so a live block disarms the self-funded
		// exemption exactly as on the repo-bots API.
		gateOrg, gerr := s.provisionOrgRequiringApproval(r.Context(), id, teamID, cfg.Pool != nil && cfg.Pool.Enabled)
		if gerr != nil {
			httpError(w, http.StatusServiceUnavailable, "provision-approval gate unavailable: %v", gerr)
			return
		}
		if gateOrg != "" && webhookPatchExpandsSurface(cfg, req) {
			httpError(w, http.StatusConflict,
				"this org requires approval for expanding a managed integration's automation — request the change through the repo-bots API (/api/teams/{id}/forge/repo-bots), which queues it for an org admin")
			return
		}
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
	if req.LabelAllowlist != nil {
		cfg.LabelAllowlist = req.LabelAllowlist
	}
	// A non-nil empty slice is how the operator turns the hold off again —
	// distinct from the field being absent, which leaves it untouched.
	if req.HoldLabels != nil {
		cfg.HoldLabels = req.HoldLabels
	}
	if req.ReviewOnSync != nil {
		cfg.ReviewOnSync = *req.ReviewOnSync
		// An explicit set is an operator decision: pin it so the gating
		// derivation stops rewriting the field at the next provision (in
		// either direction — see webhooks.Config.ReviewOnSyncPinned).
		cfg.ReviewOnSyncPinned = true
	}
	if req.ReviewOnSyncPinned != nil {
		cfg.ReviewOnSyncPinned = *req.ReviewOnSyncPinned
	}
	if req.Overlap != nil {
		// Already trimmed and normalized above (the clear reads "allow").
		cfg.Overlap = *req.Overlap
		if err := schedgate.Validate(cfg.OverlapPolicy()); err != nil {
			httpError(w, http.StatusBadRequest, "%v", err)
			return
		}
	}
	if req.AutoImplementOnOpen != nil {
		cfg.AutoImplementOnOpen = *req.AutoImplementOnOpen
	}
	if req.BranchImproveAsPR != nil {
		cfg.BranchImproveAsPR = *req.BranchImproveAsPR
	}
	if req.RateLimit != nil {
		cfg.RateLimit = *req.RateLimit
		cfg.RateLimitPinned = true
	}
	if req.MonthlyCallLimit != nil {
		cfg.MonthlyCallLimit = *req.MonthlyCallLimit
	}
	if req.LaunchVars != nil {
		cfg.LaunchVars = req.LaunchVars
	}
	if req.KeyOverrides != nil {
		if err := s.validateKeyOverrides(teamTenantCtx(r.Context(), teamID), teamID, webhookActor(cfg.ID), req.KeyOverrides); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		cfg.KeyOverrides = req.KeyOverrides
	}
	if req.SecretOverrides != nil {
		if err := s.validateSecretOverrides(teamTenantCtx(r.Context(), teamID), teamID, req.SecretOverrides); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		cfg.SecretOverrides = req.SecretOverrides
	}
	if req.AuthorizedRepliers != nil {
		cfg.AuthorizedRepliers = req.AuthorizedRepliers
	}
	if req.ReviewRequestLogins != nil {
		cfg.ReviewRequestLogins = req.ReviewRequestLogins
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
	if req.ConnectorConnectionID != nil {
		// An explicit empty string clears the binding — the webhook returns
		// to the legacy provider lane.
		if *req.ConnectorConnectionID != "" {
			if err := s.validateConnectorBinding(r.Context(), teamID, cfg.Provider, *req.ConnectorConnectionID); err != nil {
				httpError(w, http.StatusBadRequest, "%s", err.Error())
				return
			}
		}
		cfg.ConnectorConnectionID = *req.ConnectorConnectionID
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
		if errors.Is(err, webhooks.ErrConflict) {
			httpError(w, http.StatusConflict, "%v", err)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// Same closure as the create route: the landed pins are re-checked
	// against their records — a pin that validated while its record lived
	// but landed after the record's delete sweep listed would otherwise
	// stand unreconciled (see credential_pin_sweep.go). The re-check reads
	// every pin the stored row holds, the ones this patch left untouched
	// (never validated above) included. Unlike the create, a failed
	// re-check answers 5xx here: the edit is stored and nothing one-time is
	// lost, and the mend is the retry's own pass of this re-check, not a
	// validation — only a pin map the retry re-sends is validated again
	// (reconcileLandedPins).
	guardCtx, guardCancel := pinGuardCtx(r)
	defer guardCancel()
	pins := s.reconcileLandedPins(guardCtx, teamID, cfg.KeyOverrides, cfg.SecretOverrides)
	meta := map[string]any{"name": cfg.Name, "enabled": cfg.Enabled}
	if len(pins.Cleared) > 0 {
		meta["pins_cleared"] = pins.Cleared
	}
	if pins.Err != nil {
		meta["pins_unreconciled"] = pins.Unreconciled
	}
	s.auditTenant(r, teamID, "webhook.updated", "webhook", cfg.ID, meta)
	if pins.Err != nil {
		httpError(w, http.StatusInternalServerError, "webhook %s updated, but re-checking the records its pins name failed: %v — a pin may name a record deleted in flight and stand until it is cleared", cfg.ID, pins.Err)
		return
	}
	// The answer must not resell the swept pins — the honesty the delete's
	// 204 owes: answer with the row as the store now holds it.
	pins.strip(cfg.KeyOverrides)
	pins.strip(cfg.SecretOverrides)
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
		// Integrations tab instead (a new endpoint gets a distinct identity).
		httpError(w, http.StatusConflict, "this webhook is managed by a forge integration — rotate it by disabling and re-enabling the bot in the Integrations tab")
		return
	}
	plaintext, hash, last4, fp, err := webhooks.MintToken(r.Context())
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
		if errors.Is(err, webhooks.ErrConflict) {
			httpError(w, http.StatusConflict, "%v", err)
			return
		}
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
