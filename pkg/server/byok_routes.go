package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// registerBYOKRoutes wires every /api/teams/:id/api-keys and
// /api/me/api-keys endpoint. Called from routes() when an
// ApiKeyStore is wired.
func (s *Server) registerBYOKRoutes() {
	s.mux.Handle("GET /api/teams/{id}/api-keys", s.requireAuth(http.HandlerFunc(s.handleListTeamApiKeys)))
	s.mux.Handle("POST /api/teams/{id}/api-keys", s.requireAuth(http.HandlerFunc(s.handleCreateTeamApiKey)))
	s.mux.Handle("PATCH /api/teams/{id}/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateApiKey)))
	s.mux.Handle("DELETE /api/teams/{id}/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteApiKey)))

	s.mux.Handle("GET /api/me/api-keys", s.requireAuth(http.HandlerFunc(s.handleListMyApiKeys)))
	s.mux.Handle("POST /api/me/api-keys", s.requireAuth(http.HandlerFunc(s.handleCreateMyApiKey)))
	s.mux.Handle("PATCH /api/me/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateApiKey)))
	s.mux.Handle("DELETE /api/me/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteApiKey)))
}

type apiKeyView struct {
	ID          string  `json:"id"`
	Provider    string  `json:"provider"`
	Name        string  `json:"name"`
	Last4       string  `json:"last4,omitempty"`
	Fingerprint string  `json:"fingerprint,omitempty"`
	IsDefault   bool    `json:"is_default"`
	ScopeUserID string  `json:"scope_user_id,omitempty"`
	CreatedAt   string  `json:"created_at"`
	LastUsedAt  *string `json:"last_used_at,omitempty"`
	// 0 = uncapped; see secrets.ApiKey.MaxConcurrentRuns.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// AliveRuns is how many runs currently count against this key's
	// ceiling — alive, stamped with its fingerprint, and executing a model
	// node (docs/byok.md, "what counts"). Reported whatever the ceiling
	// is, so "is this key in use right now?" has an answer. Absent when
	// the count is unavailable (no fingerprint on a legacy row, no run
	// store, a store error — logged), never a silent zero.
	AliveRuns *int `json:"alive_runs,omitempty"`
	// RefusedUntil / RefusedReason report that the PROVIDER is currently
	// turning this credential away — a dead token, a fair-usage limit, a
	// spent org ceiling, an exhausted window. The launch walk acts on the
	// same evidence (it skips the key, or honours a webhook pin over it);
	// without these the human reading this view had no way to see it, and
	// a pinned refused key was visible only as the absence of a log line.
	// Absent when nothing is refusing the key, and on a row with no
	// fingerprint (which names a slot, not a credential).
	RefusedUntil  *string `json:"refused_until,omitempty"`
	RefusedReason string  `json:"refused_reason,omitempty"`
	// Bots is the key's workload audience — empty means every bot. Always
	// emitted, including empty, because "this key funds everything" is the
	// answer a reader most needs and the one an omitted field hides.
	Bots []string `json:"bots"`
	// Repos is the key's repository audience, same contract as Bots.
	Repos []string `json:"repos"`
	// BaseURL is the gateway root of an openai_compatible key; absent on
	// every other provider.
	BaseURL string `json:"base_url,omitempty"`
}

type createApiKeyReq struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	IsDefault bool   `json:"is_default,omitempty"`
	// MaxConcurrentRuns caps how many alive runs may hold this key at
	// once (0 = uncapped) — the operator-side answer to providers whose
	// fair-usage limits publish no numeric bound.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// Bots restricts which bot ids may draw on this key. Omitted or empty
	// admits every bot — see secrets.ApiKey.Bots.
	Bots []string `json:"bots,omitempty"`
	// Repos restricts which repositories ("owner/repo") may spend this key.
	Repos []string `json:"repos,omitempty"`
	// BaseURL is required for provider openai_compatible and refused on
	// every other provider.
	BaseURL string `json:"base_url,omitempty"`
}

type updateApiKeyReq struct {
	Name              *string `json:"name,omitempty"`
	IsDefault         *bool   `json:"is_default,omitempty"`
	Secret            *string `json:"secret,omitempty"` // rotate
	MaxConcurrentRuns *int    `json:"max_concurrent_runs,omitempty"`
	// Bots replaces the audience wholesale. A pointer to a nil-or-empty
	// slice CLEARS it (back to "every bot"); an absent field leaves it
	// alone — the distinction `omitempty` on a plain slice cannot make,
	// and the one that decides whether an audience can ever be lifted.
	Bots *[]string `json:"bots,omitempty"`
	// Repos replaces the repository audience wholesale, as Bots does.
	Repos *[]string `json:"repos,omitempty"`
	// BaseURL moves an openai_compatible key to another gateway. A move is
	// a rotation: Secret is required in the same request and is sealed
	// under the new URL (the stored secret is never re-sealed for a host
	// its holder did not prove it for), guarded against a concurrent
	// writer; refused on every other provider, blank included.
	BaseURL *string `json:"base_url,omitempty"`
}

func (s *Server) toApiKeyView(ctx context.Context, k secrets.ApiKey) apiKeyView {
	v := apiKeyView{
		ID:          k.ID,
		Provider:    string(k.Provider),
		Name:        k.Name,
		Last4:       k.Last4,
		Fingerprint: k.Fingerprint,
		IsDefault:   k.IsDefault,
		ScopeUserID: k.ScopeUserID,
		CreatedAt:   k.CreatedAt.Format(time.RFC3339),
		LastUsedAt:  optRFC3339(k.LastUsedAt),

		MaxConcurrentRuns: k.MaxConcurrentRuns,
		AliveRuns:         s.aliveRunsFor(ctx, k),
		Bots:              append([]string{}, k.Bots...),
		Repos:             append([]string{}, k.Repos...),
		BaseURL:           k.BaseURL,
	}
	if ref := s.refusalFor(ctx, k); ref.Refused() {
		until := ref.Until.UTC().Format(time.RFC3339)
		v.RefusedUntil = &until
		v.RefusedReason = ref.Reason
	}
	return v
}

// refusalFor reads what the shared ledger says about this key: is the
// provider refusing it right now, until when, and why.
//
// Keyed exactly as the launch walk keys it — the meter backend for the
// key's provider, the key's own tenant scope, the key's fingerprint — and
// folded by the same usagecap.RefusedUntil, so the view can only ever
// report a state the walk would act on. Zero value on every uncertainty (no
// ledger, no fingerprint, a provider with no metered evidence, a store
// error): a view is not worth failing a request for.
func (s *Server) refusalFor(ctx context.Context, k secrets.ApiKey) usagecap.Refusal {
	if s.usageCaps == nil || k.Fingerprint == "" {
		return usagecap.Refusal{}
	}
	backend := usageMeterBackendForProvider(k.Provider)
	if backend == "" {
		return usagecap.Refusal{}
	}
	readings, err := s.usageCaps.Latest(ctx, usagecap.Key(backend, meterScopeForKeyScope(k.ScopeTeamID), k.Fingerprint))
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("byok: usage-reading lookup for key %s: %v", k.ID, err)
		}
		return usagecap.Refusal{}
	}
	return usagecap.RefusedUntil(readings, time.Now(), s.usageCapTrust)
}

// meterScopeForKeyScope maps a key's STORE scope to the usage-cap meter
// scope, so a view and the launch walk read the same ledger.
//
// The two reserved scopes are the whole reason it exists: a platform key
// meters on the deployment's single meter and an org key on its org's, and
// wrapping either in TenantScope would key the ledger under a literal no
// run ever writes — the view would then report "never refused" for a
// credential the walk is actively skipping. The publisher's own three call
// sites pass their scope explicitly (see fillFromPlatform / fillFromOrg);
// this is the read side of the same mapping.
func meterScopeForKeyScope(scopeTeamID string) string {
	if scopeTeamID == secrets.PlatformTenantID {
		return usagecap.ScopePlatform
	}
	if orgID, ok := secrets.OrgIDFromTierScope(scopeTeamID); ok {
		return usagecap.OrgScope(orgID)
	}
	return usagecap.TenantScope(scopeTeamID)
}

// usageMeterBackendForProvider names the meter backend a provider's
// refusals are recorded under, "" for one that carries no metered evidence.
// Deferred to the delegate that does the metering: "kept identical on
// purpose" was a promise made twice in two packages, and the day they drift
// this view reports "never refused" for a credential the launch walk is
// actively skipping.
func usageMeterBackendForProvider(prov secrets.Provider) string {
	return delegate.UsageMeterBackendForProvider(prov)
}

// aliveRunsFor counts the runs holding k's concurrency slot right now —
// the same query the launch walk's ceiling asks, so the view and the gate
// can never disagree. nil when there is nothing to count with.
func (s *Server) aliveRunsFor(ctx context.Context, k secrets.ApiKey) *int {
	if s.cfg.Store == nil || k.Fingerprint == "" {
		return nil
	}
	n, err := s.cfg.Store.CountAliveRunsWithCredFingerprint(ctx, k.Fingerprint, "")
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("byok: alive-run count for key %s: %v", k.ID, err)
		}
		return nil
	}
	return &n
}

// writeApiKeyList serialises a slice of ApiKey records into the
// {"keys":[...]} envelope shared by the team and user-scoped list
// endpoints. On error it writes a 500 and returns false so the
// caller can early-out.
func (s *Server) writeApiKeyList(w http.ResponseWriter, r *http.Request, keys []secrets.ApiKey, err error) bool {
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return false
	}
	views := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, s.toApiKeyView(r.Context(), k))
	}
	writeJSON(w, struct {
		Keys []apiKeyView `json:"keys"`
	}{Keys: views})
	return true
}

// apiKeyScopeCtx picks the store scope for the api-key routes. It is a thin
// adapter over teamPathTenantCtx/teamTenantCtx rather than a second
// implementation: scopeOverride names the scope for routes whose {id} is NOT
// one — the org credential tier, where {id} is an ORG id while the rows live
// under the reserved secrets.OrgTierTenantID(orgID). Scoping by the path
// there would filter on an id no row carries, so the org's own key would
// read as absent rather than as an error.
func apiKeyScopeCtx(r *http.Request, scopeOverride string) context.Context {
	if scopeOverride != "" {
		return teamTenantCtx(r.Context(), scopeOverride)
	}
	return teamPathTenantCtx(r)
}

// auditApiKey routes an api-key mutation to the right audit log. A row
// scoped to a RESERVED literal is not tenant BYOK and must not be filed as
// such — a tenant-scoped entry under a sentinel would be readable by
// nobody: platform rows are super-admin actions on the deployment's own
// fallback credentials and land in the PLATFORM log, org-tier rows are
// org-admin actions on the org's shared credentials and land in that ORG's
// log. Every other row is ordinary tenant BYOK.
func (s *Server) auditApiKey(r *http.Request, teamID, suffix, keyID string, meta map[string]any) {
	if teamID == secrets.PlatformTenantID {
		s.auditPlatform(r, "", "platform.llm_key."+suffix, "platform_llm_key", keyID, meta)
		return
	}
	if orgID, ok := secrets.OrgIDFromTierScope(teamID); ok {
		s.auditOrg(r, orgID, "org.llm_key."+suffix, "org_llm_key", keyID, meta)
		return
	}
	s.auditTenant(r, teamID, "byok."+suffix, "byok", keyID, meta)
}

func (s *Server) handleListTeamApiKeys(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	// Team admins see all team-wide keys + their own user-scoped
	// keys (matches BYOK plan). Members only see what's visible.
	keys, err := s.apiKeys.ListByTeam(teamPathTenantCtx(r), teamID, id.UserID)
	s.writeApiKeyList(w, r, keys, err)
}

func (s *Server) handleCreateTeamApiKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	s.handleCreateApiKey(w, r, teamID, "")
}

func (s *Server) handleListMyApiKeys(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id.TeamID == "" {
		httpError(w, http.StatusBadRequest, "no active team")
		return
	}
	keys, err := s.apiKeys.ListByUser(r.Context(), id.TeamID, id.UserID)
	s.writeApiKeyList(w, r, keys, err)
}

func (s *Server) handleCreateMyApiKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id.TeamID == "" {
		httpError(w, http.StatusBadRequest, "no active team")
		return
	}
	s.handleCreateApiKey(w, r, id.TeamID, id.UserID)
}

// handleCreateApiKey is the shared write path used by both team and
// user creation endpoints. teamID is always set; userID is "" for
// team-scoped keys.
func (s *Server) handleCreateApiKey(w http.ResponseWriter, r *http.Request, teamID, userID string) {
	id, _ := auth.FromContext(r.Context())
	var req createApiKeyReq
	// Strict: a field this build does not know (a newer client's column)
	// must be refused, never silently dropped — a silently dropped base_url
	// is a gateway key that will be sent nowhere.
	if err := readJSONStrict(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body: %v", err)
		return
	}
	provider, err := secrets.ParseProvider(req.Provider)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if req.Secret == "" || req.Name == "" {
		httpError(w, http.StatusBadRequest, "name + secret required")
		return
	}
	// Ingestion gate — reject a pasted transcript / credentials.json blob
	// before it lands in the store, the same backstop as sealOAuthRecord.
	// Per provider: a bearer token has no whitespace or control character;
	// Bedrock/Vertex carry a JSON credential document instead.
	if err := secrets.ValidateAPIKeyShape(provider, req.Secret); err != nil {
		s.refuseApiKey(w, r, teamID, "", provider, err)
		return
	}
	if req.MaxConcurrentRuns < 0 {
		httpError(w, http.StatusBadRequest, "max_concurrent_runs must be >= 0 (0 = uncapped)")
		return
	}
	bots, berr := normalizeBotAudience(req.Bots)
	if berr != nil {
		httpError(w, http.StatusBadRequest, "%s", berr.Error())
		return
	}
	repos, rerr := normalizeRepoAudience(req.Repos)
	if rerr != nil {
		httpError(w, http.StatusBadRequest, "%s", rerr.Error())
		return
	}
	baseURL, uerr := gatewayBaseURLFor(provider, req.BaseURL)
	if uerr != nil {
		s.refuseApiKey(w, r, teamID, "", provider, uerr)
		return
	}
	keyID := secrets.NewApiKeyID()
	now := time.Now().UTC()
	key := secrets.ApiKey{
		ID:          keyID,
		ScopeTeamID: teamID,
		ScopeUserID: userID,
		Provider:    provider,
		Name:        req.Name,
		Last4:       secrets.Last4(req.Secret),
		IsDefault:   req.IsDefault,
		CreatedBy:   id.UserID,
		CreatedAt:   now,
		Fingerprint: secrets.APIKeyFingerprint(provider, req.Secret, baseURL),

		MaxConcurrentRuns: req.MaxConcurrentRuns,
		Bots:              bots,
		Repos:             repos,
		BaseURL:           baseURL,
	}
	sealed, err := secrets.SealAPIKeyFor(s.sealer, key, []byte(req.Secret))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal: %v", err)
		return
	}
	key.SealedSecret = sealed
	// The EXPLICIT scope, not the path: on the org-credential route {id}
	// is an org id while the row belongs under the reserved org-tier
	// scope. teamID is already the right value on all three callers.
	ctx := teamTenantCtx(r.Context(), teamID)
	if err := s.apiKeys.Create(ctx, key); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if req.IsDefault {
		if err := s.apiKeys.ClearDefault(ctx, teamID, userID, provider, keyID); err != nil {
			httpError(w, http.StatusInternalServerError, "key %s created but clearing previous default failed: %v", keyID, err)
			return
		}
	}
	meta := map[string]any{"name": key.Name, "provider": string(provider), "user_scoped": userID != ""}
	if key.BaseURL != "" {
		meta["gateway_host"] = key.BaseURL
	}
	s.auditApiKey(r, teamID, "created", keyID, meta)
	writeJSON(w, s.toApiKeyView(r.Context(), key))
}

// maxBotAudienceEntries bounds a key's workload audience. An allow-list is
// read on every credential resolution, so it is not a place for an unbounded
// operator-supplied list; the ceiling is far above any real deployment's bot
// roster and exists to make the refusal explicit rather than to ration.
const maxBotAudienceEntries = 128

// maxRepoPathBytes bounds one entry of a repository audience. A forge path
// is tens of bytes; the ceiling exists so that a refusal names a size
// rather than a store swallowing a pasted document as a repository.
const maxRepoPathBytes = 1024

// normalizeRepoAudience canonicalises a repository audience. An entry is the
// forge's full path of ONE repository — "owner/repo", or "group/sub/project"
// on a forge with sub-groups — trimmed, lower-cased (the resolver compares
// case-insensitively; the canonical form keeps the list readable), with a
// surrounding "/" and every ".git" suffix dropped, deduplicated. It refuses
// what syntax alone can tell is not such a path (a URL, a glob, whitespace,
// a control character, an empty or dot segment), so that a key restricted
// to such an entry does not quietly fund nothing: the launch carries the
// path the forge reports. A host prefix ("github.com/o/r") or a browser
// path ("o/r/tree/main") is NOT told apart — a forge group may be named
// like a host — and such a key funds nothing until the entry is fixed; the
// preview shows it withheld. Like the bot audience, a list of blanks is a
// misfire. A refusal names the entry's position, not its value.
func normalizeRepoAudience(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxBotAudienceEntries {
		return nil, fmt.Errorf("repos: at most %d entries (got %d)", maxBotAudienceEntries, len(in))
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i, raw := range in {
		rp := strings.Trim(strings.TrimSpace(raw), "/")
		if rp == "" {
			continue
		}
		if len(rp) > maxRepoPathBytes {
			return nil, fmt.Errorf("repos[%d]: longer than %d bytes", i, maxRepoPathBytes)
		}
		rp = strings.ToLower(rp)
		for strings.HasSuffix(rp, ".git") {
			rp = strings.TrimSuffix(rp, ".git")
		}
		if err := checkRepoPath(rp); err != nil {
			return nil, fmt.Errorf("repos[%d]: %w", i, err)
		}
		if seen[rp] {
			continue
		}
		seen[rp] = true
		out = append(out, rp)
	}
	if len(out) == 0 {
		return nil, errors.New("repos: every entry is blank — send an empty list to clear the audience, not a list of blanks")
	}
	return out, nil
}

// checkRepoPath accepts a lower-cased forge path: two or more "/"-separated
// segments of letters, digits, ".", "_" and "-", none empty, "." or "..".
// A scheme, a glob, whitespace or a control character is refused with the
// reason named, since each is a way to write something that looks like a
// repository and matches none.
func checkRepoPath(rp string) error {
	if strings.Contains(rp, "://") {
		return errors.New("is a URL, not a repository path (owner/repo)")
	}
	segs := strings.Split(rp, "/")
	if len(segs) < 2 {
		return errors.New("is not a repository path (owner/repo)")
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("has an empty, \".\" or \"..\" segment")
		}
		for _, r := range seg {
			if !repoPathRune(r) {
				return errors.New("carries a character outside letters, digits, \".\", \"_\" and \"-\" (a glob, a host or a URL is not a repository path)")
			}
		}
	}
	return nil
}

// repoPathRune accepts the characters a lower-cased forge path segment is
// made of.
func repoPathRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
}

// gatewayBaseURLFor validates a create's base_url against the provider:
// required and canonicalised for openai_compatible, refused (as a shape
// error, audited by the caller) on every other provider, whose endpoint is
// compiled in. The update path reads the row's provider and has its own
// rule (a present field is refused on a vendor row, blank included).
func gatewayBaseURLFor(provider secrets.Provider, raw string) (string, error) {
	if provider == secrets.ProviderOpenAICompatible {
		return secrets.ValidateGatewayBaseURL(raw)
	}
	if strings.TrimSpace(raw) != "" {
		return "", &secrets.ShapeError{Field: "base_url", Reason: fmt.Sprintf("is only accepted for provider openai_compatible (provider %s has a compiled endpoint)", provider)}
	}
	return "", nil
}

// normalizeBotAudience trims, de-duplicates and validates a submitted
// workload audience.
//
// It refuses rather than repairs in the one case that matters: a list the
// caller filled with nothing but blanks. Dropping those silently would turn
// "only these bots" into "every bot" — the exact widening the field exists
// to prevent — and an operator who mistyped would learn it from a key that
// funds everything. An ABSENT or genuinely empty list is a different thing
// and stays legal: that is how an audience is lifted.
func normalizeBotAudience(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxBotAudienceEntries {
		return nil, fmt.Errorf("bots: at most %d entries (got %d)", maxBotAudienceEntries, len(in))
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		// The canonical form botregistry imposes everywhere a bot is named,
		// so an operator writing `Feature_Dev` scopes the same bot the
		// launch path resolves to `feature-dev`. The resolver compares
		// exactly; canonicalising is this edge's job, not its job.
		b := botregistry.NormalizeName(raw)
		if b == "" {
			continue
		}
		if seen[b] {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, errors.New("bots: every entry is blank — send an empty list to clear the audience, not a list of blanks")
	}
	return out, nil
}

// refuseApiKey answers a BYOK shape refusal (create or rotate): 400 with
// the reason, plus the trace the success path already leaves — a Warn
// and an audit event naming the provider, the field and the reason,
// never the value — so a paste that would have burned a fleet of runs
// on 401s is findable after the fact. keyID is empty on a create.
func (s *Server) refuseApiKey(w http.ResponseWriter, r *http.Request, teamID, keyID string, provider secrets.Provider, err error) {
	field, reason := "secret", err.Error()
	var se *secrets.ShapeError
	if errors.As(err, &se) {
		field, reason = se.Field, se.Reason
	}
	s.logger.Warn("byok: team=%s key=%q provider=%s credential REFUSED at ingestion (field=%s): %s", teamID, keyID, provider, field, reason)
	s.auditApiKey(r, teamID, "refused", keyID, map[string]any{"provider": string(provider), "field": field, "reason": reason})
	httpError(w, http.StatusBadRequest, "%s", err.Error())
}

func (s *Server) handleUpdateApiKey(w http.ResponseWriter, r *http.Request) {
	s.handleUpdateApiKeyIn(w, r, "")
}

// handleUpdateApiKeyIn is the shared update path; scopeOverride names the
// store scope for routes whose {id} is not one (the org tier).
func (s *Server) handleUpdateApiKeyIn(w http.ResponseWriter, r *http.Request, scopeOverride string) {
	id, _ := auth.FromContext(r.Context())
	keyID := r.PathValue("key_id")
	ctx := apiKeyScopeCtx(r, scopeOverride)
	key, err := s.apiKeys.Get(ctx, keyID)
	if err != nil {
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			httpError(w, http.StatusNotFound, "key not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// AuthZ: team-wide keys → whoever may manage that team, which is the
	// same right that created the key. User-scoped keys → only the owning
	// user (or super-admin).
	if !s.canMutateApiKey(r.Context(), id, key) {
		httpError(w, http.StatusForbidden, "cannot mutate this key")
		return
	}
	// A row whose provider this build does not know was written by a newer
	// server (a rolling update, or the one this pod was rolled back from).
	// Its provider-specific columns are invisible to this struct, and the
	// shape rule for its secret is unknown here (ValidateAPIKeyShape would
	// fall back to the bearer-token rule), so a rotate from here could store
	// a value the provider never accepts. Refuse the mutation outright;
	// deleting the row stays possible from any build.
	if !key.Provider.Valid() {
		s.logger.Warn("byok: team=%s key=%q provider=%s update REFUSED: provider unknown to this server build", key.ScopeTeamID, key.ID, key.Provider)
		s.auditApiKey(r, key.ScopeTeamID, "refused", key.ID, map[string]any{"provider": string(key.Provider), "field": "provider", "reason": "unknown to this server build"})
		httpError(w, http.StatusConflict, "key %s names provider %q, which this server build does not know — update it from a current server, or delete it", key.ID, key.Provider)
		return
	}
	var req updateApiKeyReq
	if err := readJSONStrict(r, &req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body: %v", err)
		return
	}
	// Only the fields the request names reach the store: the copy read
	// above is already stale on everything else (a ClearDefault or a
	// MarkUsed may have landed in between), and the row may carry columns
	// this build does not know.
	patch := secrets.ApiKeyPatch{Name: req.Name, IsDefault: req.IsDefault}
	rotated := req.Secret != nil && *req.Secret != ""
	// The record the new blob is sealed FOR: the current row, repointed
	// when the request moves base_url. A move is a ROTATION — the caller
	// re-sends the secret and it is sealed under the new URL in the same
	// write. The stored blob is never opened to follow a URL: a credential
	// is write-only, and the host it is sent to changes only with proof of
	// possession, or any principal with mutate rights (a team admin, an org
	// admin who never saw the key) could redirect it to a host of theirs.
	target := key
	if req.BaseURL != nil {
		if key.Provider != secrets.ProviderOpenAICompatible {
			// Any value, blank included: a vendor row has no URL column to
			// set, and a 200 that wrote nothing would read as accepted.
			s.refuseApiKey(w, r, key.ScopeTeamID, key.ID, key.Provider, &secrets.ShapeError{Field: "base_url", Reason: fmt.Sprintf("is only accepted for provider openai_compatible (provider %s has a compiled endpoint)", key.Provider)})
			return
		}
		u, uerr := secrets.ValidateGatewayBaseURL(*req.BaseURL)
		if uerr != nil {
			s.refuseApiKey(w, r, key.ScopeTeamID, key.ID, key.Provider, uerr)
			return
		}
		if u != key.BaseURL {
			if !rotated {
				s.refuseApiKey(w, r, key.ScopeTeamID, key.ID, key.Provider, &secrets.ShapeError{Field: "base_url", Reason: "moves the key to another gateway: send the secret in the same request — the stored secret is never re-sealed for a host its holder did not prove it for"})
				return
			}
			target.BaseURL = u
		}
	}
	if rotated {
		// Same ingestion gate as create — a rotate that pastes a transcript
		// is exactly as bad as a create that pastes one, and the runtime
		// error is identical (401 on every call). Refuse it here.
		if err := secrets.ValidateAPIKeyShape(key.Provider, *req.Secret); err != nil {
			s.refuseApiKey(w, r, key.ScopeTeamID, key.ID, key.Provider, err)
			return
		}
		plaintext := []byte(*req.Secret)
		sealed, err := secrets.SealAPIKeyFor(s.sealer, target, plaintext)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "seal: %v", err)
			return
		}
		patch.Secret = &secrets.ApiKeySecretPatch{
			Sealed:      sealed,
			Last4:       secrets.Last4(string(plaintext)),
			Fingerprint: secrets.APIKeyFingerprint(key.Provider, string(plaintext), target.BaseURL),
		}
		// Conditional on the blob read above: a rotation prepared from a
		// stale read would otherwise land last and silently undo what a
		// concurrent writer did in between — another rotation, or a move
		// away from a gateway the operator no longer trusts, reverted here
		// to the old host with a fresh secret. The row stays openable
		// either way (the URL below travels with the blob); the guard is
		// about not losing the other writer's change.
		patch.ExpectSealed = key.SealedSecret
		if key.Provider == secrets.ProviderOpenAICompatible {
			// The URL the blob is sealed under travels with the blob: the
			// store refuses one without the other.
			baseURL := target.BaseURL
			patch.BaseURL = &baseURL
		}
	}
	if req.Repos != nil {
		repos, rerr := normalizeRepoAudience(*req.Repos)
		if rerr != nil {
			httpError(w, http.StatusBadRequest, "%s", rerr.Error())
			return
		}
		if repos == nil {
			repos = []string{}
		}
		patch.Repos = &repos
	}
	if req.MaxConcurrentRuns != nil {
		if *req.MaxConcurrentRuns < 0 {
			httpError(w, http.StatusBadRequest, "max_concurrent_runs must be >= 0 (0 = uncapped)")
			return
		}
		patch.MaxConcurrentRuns = req.MaxConcurrentRuns
	}
	if req.Bots != nil {
		bots, berr := normalizeBotAudience(*req.Bots)
		if berr != nil {
			httpError(w, http.StatusBadRequest, "%s", berr.Error())
			return
		}
		patch.Bots = &bots
	}
	updated, err := s.apiKeys.Patch(ctx, key.ID, patch)
	if err != nil {
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			httpError(w, http.StatusNotFound, "key not found")
			return
		}
		if errors.Is(err, secrets.ErrApiKeyChanged) {
			httpError(w, http.StatusConflict, "key %s was rotated or moved by another writer while this update was prepared — read it again and retry", key.ID)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// Only a request that PROMOTES this key clears the others: a rename of
	// the current default must not race a concurrent promotion of another
	// key and erase it (updated is already stale by the time it is read).
	if req.IsDefault != nil && *req.IsDefault && updated.IsDefault {
		if err := s.apiKeys.ClearDefault(ctx, updated.ScopeTeamID, updated.ScopeUserID, updated.Provider, updated.ID); err != nil {
			httpError(w, http.StatusInternalServerError, "key updated but clearing previous default failed: %v", err)
			return
		}
	}
	umeta := map[string]any{"name": updated.Name, "rotated": rotated}
	// What THIS request moved, not what the row happens to hold: a rename
	// racing a concurrent move must not be audited as the move.
	if target.BaseURL != key.BaseURL {
		umeta["gateway_host"] = target.BaseURL
	}
	s.auditApiKey(r, updated.ScopeTeamID, "updated", updated.ID, umeta)
	writeJSON(w, s.toApiKeyView(r.Context(), updated))
}

func (s *Server) handleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	s.handleDeleteApiKeyIn(w, r, "")
}

// handleDeleteApiKeyIn is the shared delete path; see handleUpdateApiKeyIn.
func (s *Server) handleDeleteApiKeyIn(w http.ResponseWriter, r *http.Request, scopeOverride string) {
	id, _ := auth.FromContext(r.Context())
	keyID := r.PathValue("key_id")
	ctx := apiKeyScopeCtx(r, scopeOverride)
	key, err := s.apiKeys.Get(ctx, keyID)
	if err != nil {
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if !s.canMutateApiKey(r.Context(), id, key) {
		httpError(w, http.StatusForbidden, "cannot delete this key")
		return
	}
	if err := s.apiKeys.Delete(ctx, key.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditApiKey(r, key.ScopeTeamID, "deleted", key.ID, map[string]any{"name": key.Name, "provider": string(key.Provider)})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) canMutateApiKey(ctx context.Context, id auth.Identity, k secrets.ApiKey) bool {
	return s.canMutateScopedRecord(ctx, id, k.ScopeUserID, k.ScopeTeamID)
}
