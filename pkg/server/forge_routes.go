package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ForgeGitHubAppConfig is the global GitHub-App identity (registered once on
// GitHub), used for the installation-token connect mode. Empty/unconfigured
// → the GitHub-App connect path is unavailable (OAuth/PAT still work).
type ForgeGitHubAppConfig struct {
	AppID      int64
	PrivateKey string // PEM
	AppSlug    string // for the install URL github.com/apps/<slug>/installations/new
	// ClientID/ClientSecret are the App's user-authorization OAuth
	// credentials (ITERION_FORGE_GITHUB_APP_CLIENT_ID/_CLIENT_SECRET).
	// Optional. When set — and the App has "Request user authorization
	// during installation" enabled — the install callback verifies the
	// completing user owns the installation before minting a token (closes
	// the installation_id IDOR on the shared-app path).
	ClientID     string
	ClientSecret string
}

func (c ForgeGitHubAppConfig) Configured() bool {
	return c.AppID != 0 && strings.TrimSpace(c.PrivateKey) != ""
}

func (s *Server) githubAppConfig() forgegithub.AppConfig {
	return forgegithub.AppConfig{
		AppID:         s.forgeGitHubApp.AppID,
		PrivateKeyPEM: s.forgeGitHubApp.PrivateKey,
		AppSlug:       s.forgeGitHubApp.AppSlug,
		ClientID:      s.forgeGitHubApp.ClientID,
		ClientSecret:  s.forgeGitHubApp.ClientSecret,
	}
}

// forgeAgentBindingCookie is the per-flow CSRF-binding cookie for the
// forge OAuth connect flow (the analogue of oidcAgentBindingCookie).
const forgeAgentBindingCookie = "iterion_forge_agent"

// forgePending is the server-side state held between the forge OAuth
// /connect and /callback. Unlike oidc.PendingAuth it carries the tenant +
// forge base URL, because the callback (a public IdP redirect) resolves
// the team from the signed state, not from a path or JWT.
type forgePending struct {
	State        string
	CodeVerifier string
	Provider     forge.Provider
	ForgeBaseURL string
	TenantID     string
	UserID       string
	AgentBinding string
	NextURL      string
	IssuedAt     time.Time
}

// forgeStateBackend stores forgePending CSRF state keyed by State, with a
// one-time `take`. The in-memory impl is single-replica; the Valkey impl
// (forge_state_valkey.go) shares it across replicas so the OAuth/manifest
// /start and /callback can land on different pods.
type forgeStateBackend interface {
	put(p forgePending)
	take(state string) (forgePending, bool)
}

// forgeStateStore is the TTL-bounded in-memory backend, mirroring
// oidc.MemoryStateStore. Used in local/desktop and single-replica deployments.
type forgeStateStore struct {
	mu  sync.Mutex
	m   map[string]forgePending
	ttl time.Duration
}

func newForgeStateStore(ttl time.Duration) *forgeStateStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &forgeStateStore{m: make(map[string]forgePending), ttl: ttl}
}

func (s *forgeStateStore) put(p forgePending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[p.State] = p
}

func (s *forgeStateStore) take(state string) (forgePending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[state]
	if !ok {
		return forgePending{}, false
	}
	delete(s.m, state)
	if time.Since(p.IssuedAt) > s.ttl {
		return forgePending{}, false
	}
	return p, true
}

// appendQueryParam sets k=v on a same-origin redirect path, preserving any
// existing query. The connect wizard resumes on ?connected=/?installed=
// after the OAuth / App-install / manifest round-trips. Called on paths
// that already went through safeNext at connect time.
func appendQueryParam(path, key, val string) string {
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	q := u.Query()
	q.Set(key, val)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) registerForgeRoutes() {
	s.mux.Handle("GET /api/teams/{id}/forge/repos", s.requireAuth(http.HandlerFunc(s.handleListTeamForgeRepos)))
	s.mux.Handle("POST /api/teams/{id}/forge/repos", s.requireAuth(http.HandlerFunc(s.handleCreateForgeRepo)))
	s.mux.Handle("GET /api/teams/{id}/forge/connections/{conn_id}/health", s.requireAuth(http.HandlerFunc(s.handleForgeConnectionHealth)))
	s.mux.Handle("GET /api/teams/{id}/forge/connections", s.requireAuth(http.HandlerFunc(s.handleListForgeConnections)))
	s.mux.Handle("POST /api/teams/{id}/forge/connections", s.requireAuth(http.HandlerFunc(s.handleConnectForge)))
	s.mux.Handle("DELETE /api/teams/{id}/forge/connections/{conn_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteForgeConnection)))
	s.mux.Handle("GET /api/teams/{id}/forge/connections/{conn_id}/repos", s.requireAuth(http.HandlerFunc(s.handleListForgeRepos)))
	// Public IdP redirect targets (see isPublicPath); authenticate via the
	// signed state + the agent-binding cookie.
	s.mux.HandleFunc("GET /api/forge/oauth/callback", s.handleForgeOAuthCallback)
	s.mux.HandleFunc("GET /api/forge/github/app/callback", s.handleForgeGitHubAppCallback)
}

// ---- factories (provider dispatch) ----

// forgeBotForge resolves a bot's manifest forge: block for the orchestrator.
func (s *Server) forgeBotForge(botID string) (*bundle.ForgeRequirements, error) {
	entry, ok, err := s.findBot(botID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bot %q not found", botID)
	}
	return entry.Forge, nil
}

// forgeBotInvocations resolves a bot's manifest invocations for the
// orchestrator's command-map build (already the EffectiveInvocations set —
// explicit block, else synthetic from a legacy forge: block).
func (s *Server) forgeBotInvocations(botID string) ([]bundle.Invocation, error) {
	entry, ok, err := s.findBot(botID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bot %q not found", botID)
	}
	return entry.Invocations, nil
}

// forgeHTTPClient is the SSRF-guarded HTTP client for ALL outbound forge
// calls (token exchange, WhoAmI, hook + app provisioning, installation-token
// mint). In strict mode (cloud, or a non-loopback-bound local server) its
// transport re-resolves and rejects any host that isn't public-unicast on
// EVERY dial — including redirect hops — so an operator-supplied self-hosted
// forge base URL can't be aimed at loopback / RFC1918 / link-local / cloud
// metadata (169.254.169.254), and DNS-rebinding can't slip past a check.
// Loopback-bound single-tenant servers stay permissive (a forge on a private
// LAN is legitimate there). Redirects ARE followed (forge APIs may 3xx); each
// hop re-enters the guarded dialer, unlike the preview proxy which must not.
//
// Built once (outboundStrict() is startup-fixed) so its transport's connection
// pool is reused across forge operations.
func (s *Server) forgeHTTPClient() *http.Client {
	s.forgeHTTPOnce.Do(func() {
		s.forgeHTTP = &http.Client{
			Timeout:   30 * time.Second,
			Transport: httpdial.SafeTransport(s.outboundStrict()),
		}
	})
	return s.forgeHTTP
}

// forgeAdminForToken builds an outbound admin client from a raw token
// (used at connect time before a Connection exists).
func (s *Server) forgeAdminForToken(provider forge.Provider, baseURL, token string) (forge.Admin, error) {
	switch provider {
	case forge.ProviderGitLab:
		return forgegitlab.New(s.forgeHTTPClient(), baseURL, token), nil
	case forge.ProviderGitHub:
		return forgegithub.New(s.forgeHTTPClient(), baseURL, token), nil
	case forge.ProviderForgejo:
		return forgeforgejo.New(s.forgeHTTPClient(), baseURL, token), nil
	default:
		return nil, fmt.Errorf("forge: provider %q is not yet supported", provider)
	}
}

// forgeAdminFor builds a connection's admin client (the orchestrator's
// AdminFor). A GitHub-App connection mints a fresh installation token from
// the App private key on demand; every other kind opens its sealed token.
func (s *Server) forgeAdminFor(ctx context.Context, conn forge.Connection) (forge.Admin, error) {
	if conn.Kind == forge.KindGitHubApp {
		cfg, _, ok := s.githubAppConfigForTenant(ctx, conn.TenantID)
		if !ok {
			return nil, fmt.Errorf("forge: no github app available for this connection")
		}
		return &forgegithub.AppClient{
			HTTP: s.forgeHTTPClient(), WebBaseURL: conn.BaseURL(),
			Cfg: cfg, InstallationID: conn.InstallationID,
		}, nil
	}
	token, err := forge.AdminTokenFor(s.sealer, conn)
	if err != nil {
		return nil, err
	}
	return s.forgeAdminForToken(conn.Provider, conn.BaseURL(), token)
}

// githubAppConfigForTenant returns the GitHub-App identity (app id + private key
// + slug) used to mint installation tokens for the least-privilege github_app
// path. It prefers the tenant's own manifest-created App (its sealed private
// key), falling back to the platform App (ITERION_FORGE_GITHUB_APP_*). ok is
// false when neither is available.
//
// shared reports whether the returned config is the SHARED platform App (the
// fallback). It matters for the install callback: a per-tenant App's private
// key is tenant-scoped, so it can only mint tokens for that tenant's own
// installations, whereas the shared App's key can mint for ANY installation —
// so the shared path (and only it) must verify installation ownership.
func (s *Server) githubAppConfigForTenant(ctx context.Context, tenantID string) (cfg forgegithub.AppConfig, shared bool, ok bool) {
	if s.forgeOAuthApps != nil {
		base := forge.CanonicalBaseURL(forge.ProviderGitHub, "")
		if app, err := s.forgeOAuthApps.GetByInstance(ctx, tenantID, forge.ProviderGitHub, base); err == nil && len(app.SealedPrivateKey) > 0 {
			if pem, err := forge.OpenForgeAppPrivateKey(s.sealer, app.ID, app.SealedPrivateKey); err == nil && pem != "" {
				if appID, _ := strconv.ParseInt(app.ProviderAppID, 10, 64); appID != 0 {
					return forgegithub.AppConfig{AppID: appID, PrivateKeyPEM: pem, AppSlug: app.AppSlug}, false, true
				}
			}
		}
	}
	if s.forgeGitHubApp.Configured() {
		return s.githubAppConfig(), true, true
	}
	return forgegithub.AppConfig{}, false, false
}

// forgeOAuthAppFor builds a provider's OAuth client for a (tenant, provider,
// instance) from the per-tenant OAuth-app store, or (nil,false) when no app is
// registered for that instance or its sealed secret can't be opened. baseURL
// is canonicalised so a connection's BaseURL() and a stored app key match.
func (s *Server) forgeOAuthAppFor(ctx context.Context, tenantID string, provider forge.Provider, baseURL string) (forge.OAuthExchanger, bool) {
	if s.forgeOAuthApps == nil {
		return nil, false
	}
	base := forge.CanonicalBaseURL(provider, baseURL)
	app, err := s.forgeOAuthApps.GetByInstance(ctx, tenantID, provider, base)
	if err != nil {
		return nil, false
	}
	secret, err := forge.OpenOAuthAppSecret(s.sealer, app.ID, app.SealedSecret)
	if err != nil {
		return nil, false
	}
	switch provider {
	case forge.ProviderGitLab:
		return &forgegitlab.OAuthApp{HTTP: s.forgeHTTPClient(), BaseURL: base, ClientID: app.ClientID, ClientSecret: secret}, true
	case forge.ProviderGitHub:
		return &forgegithub.OAuthApp{HTTP: s.forgeHTTPClient(), BaseURL: base, ClientID: app.ClientID, ClientSecret: secret}, true
	case forge.ProviderForgejo:
		return &forgeforgejo.OAuthApp{HTTP: s.forgeHTTPClient(), BaseURL: base, ClientID: app.ClientID, ClientSecret: secret}, true
	default:
		return nil, false
	}
}

func (s *Server) forgeOAuthRedirectURI() string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/api/forge/oauth/callback"
}

// forgeOAuthAppProvisioner builds the create-app client for a provider from an
// admin token. The per-provider AdminClient implements OAuthAppProvisioner
// where the forge exposes a create-app API (GitLab, Forgejo); GitHub does not
// (its create path is the interactive App-Manifest flow), so it returns a clear
// "paste an existing app instead" error here.
func (s *Server) forgeOAuthAppProvisioner(provider forge.Provider, baseURL, adminToken string) (forge.OAuthAppProvisioner, error) {
	admin, err := s.forgeAdminForToken(provider, baseURL, adminToken)
	if err != nil {
		return nil, err
	}
	prov, ok := admin.(forge.OAuthAppProvisioner)
	if !ok {
		return nil, fmt.Errorf("auto-create is not available for %s — paste an existing client_id/client_secret instead", provider)
	}
	return prov, nil
}

// forgeDefaultOAuthScopes is the scope set an auto-created OAuth app requests,
// per provider (the same defaults the connect flow uses at authorize time).
func forgeDefaultOAuthScopes(p forge.Provider) []string {
	switch p {
	case forge.ProviderGitLab:
		return forgegitlab.DefaultScopes
	case forge.ProviderGitHub:
		return forgegithub.DefaultScopes
	case forge.ProviderForgejo:
		return forgeforgejo.DefaultScopes
	default:
		return nil
	}
}

// forgeConnRepoNames returns the short repo names (GitHub installation-token
// `repositories` form — "api", not "org/api") of the repos a connection has
// provisioned, so the github_app runtime forge token is scoped to that set
// (least-privilege) instead of the whole installation.
//
// (nil, nil) legitimately means "narrow to nothing known" → whole installation
// (still minimal permissions): no integration store wired, or nothing
// provisioned yet. A non-nil error means the provisioned set could NOT be
// determined (transient store failure); callers MUST fail closed and NOT mint a
// whole-installation token in that case, or a Mongo blip would silently broaden
// the token's repo scope — the opposite of the least-privilege narrowing this
// exists for.
func (s *Server) forgeConnRepoNames(ctx context.Context, conn forge.Connection) ([]string, error) {
	if s.forgeIntegrations == nil {
		return nil, nil
	}
	ints, err := s.forgeIntegrations.ListByConnection(store.WithTenant(ctx, conn.TenantID), conn.TenantID, conn.ID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ints))
	seen := make(map[string]bool, len(ints))
	for _, ri := range ints {
		name := ri.RepoFullName
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

// forgeAppMinter mints a fresh github_app installation token scoped to the
// connection's provisioned repo set + minimal permissions — the orchestrator's
// GitHubAppMinter, used to narrow the managed forge token at provision time.
// Returns an error when no github app is available for the connection (the
// orchestrator treats that as best-effort and keeps the prior token).
func (s *Server) forgeAppMinter(ctx context.Context, conn forge.Connection) (string, error) {
	if conn.Kind != forge.KindGitHubApp {
		return "", fmt.Errorf("forge: not a github_app connection")
	}
	cfg, _, ok := s.githubAppConfigForTenant(ctx, conn.TenantID)
	if !ok {
		return "", fmt.Errorf("forge: no github app available for this connection")
	}
	// Fail closed: if the provisioned repo set can't be determined (transient
	// store error), do NOT fall back to a whole-installation token. The
	// orchestrator treats this error as best-effort and keeps the prior
	// (narrower) token rather than widening scope.
	repos, err := s.forgeMintRepoNames(ctx, conn)
	if err != nil {
		return "", fmt.Errorf("forge: cannot determine provisioned repos for least-privilege token: %w", err)
	}
	tok, _, err := forgegithub.MintInstallationToken(ctx, s.forgeHTTPClient(),
		forgegithub.APIBaseFor(conn.BaseURL()), cfg, conn.InstallationID, time.Now().UTC(),
		&forgegithub.InstallationTokenOptions{
			Repositories: repos,
			Permissions:  forgegithub.RuntimeInstallationPermissions(),
		})
	return tok, err
}

// forgeMintRepoNames is the repo-scope oracle for a github_app connection's
// least-privilege token, shared by BOTH re-mint paths — the orchestrator's
// provision-time GitHubAppMinter AND the refresh worker's AppRefresher — so
// the narrowed token stays consistent whichever re-mints it. It is
// forgeConnRepoNames (repo_integration repos) folded with the tenant's
// schedule-target repos on this connection's host.
func (s *Server) forgeMintRepoNames(ctx context.Context, conn forge.Connection) ([]string, error) {
	repos, err := s.forgeConnRepoNames(ctx, conn)
	if err != nil {
		return nil, err
	}
	return s.augmentWithScheduleRepos(ctx, conn, repos), nil
}

// augmentWithScheduleRepos unions `base` (integration repo short-names,
// installation-guaranteed) with the tenant's schedule-target repos on conn's
// host, then — because schedule repos are NOT provisioning-verified — filters
// the schedule additions to those actually in the installation (via ListRepos)
// so the mint never 422s on an unknown repo. Best-effort throughout: any error
// (store, ListRepos) returns `base` unchanged, preserving the prior behaviour
// and never widening scope beyond the installation.
func (s *Server) augmentWithScheduleRepos(ctx context.Context, conn forge.Connection, base []string) []string {
	if s.cfg.ScheduledBots == nil {
		return base
	}
	sched, err := s.cfg.ScheduledBots.ListByTenant(store.WithTenant(ctx, conn.TenantID), conn.TenantID)
	if err != nil || len(sched) == 0 {
		return base
	}
	candidates := scheduleRepoCandidates(sched, base, conn.BaseURL())
	if len(candidates) == 0 {
		return base
	}
	// Verify candidate repos are actually in the installation — a name GitHub
	// doesn't recognise 422s the whole token mint, so an unverified schedule
	// repo must never reach the Repositories list.
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return base
	}
	installRepos, err := admin.ListRepos(ctx, forge.RepoQuery{})
	if err != nil {
		return base
	}
	inInstall := make(map[string]bool, len(installRepos))
	for _, r := range installRepos {
		inInstall[shortRepoName(r.FullName)] = true
	}
	out := append([]string(nil), base...)
	for _, name := range candidates {
		if inInstall[name] {
			out = append(out, name)
		}
	}
	return out
}

// scheduleRepoCandidates returns the distinct short repo names of schedules
// whose repo_url is on `webBaseURL`'s host and not already in `base`. Pure —
// the host filter, ".git"/short-name parsing and dedup-vs-base logic that the
// installation-membership intersection then guards.
func scheduleRepoCandidates(sched []cloudsched.ScheduledBot, base []string, webBaseURL string) []string {
	have := make(map[string]bool, len(base))
	for _, n := range base {
		have[n] = true
	}
	hostPrefix := webBaseURL + "/"
	seen := make(map[string]bool)
	var out []string
	for _, sb := range sched {
		if sb.RepoURL == "" || !strings.HasPrefix(sb.RepoURL, hostPrefix) {
			continue
		}
		short := shortRepoName(sb.RepoURL)
		if short == "" || have[short] || seen[short] {
			continue
		}
		seen[short] = true
		out = append(out, short)
	}
	return out
}

// shortRepoName extracts the trailing repo segment ("owner/repo" → "repo",
// a clone/web URL → its last path element) minus any ".git" suffix. Empty
// when there is no usable segment.
func shortRepoName(s string) string {
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// forgeRefresherFor returns the token refresher for a connection, or nil
// when it cannot/should-not refresh (PAT, GitHub-App, or a provider with no
// configured OAuth app). The per-provider OAuth clients implement both
// OAuthExchanger and TokenRefresher.
func (s *Server) forgeRefresherFor(conn forge.Connection) forge.TokenRefresher {
	if conn.Kind == forge.KindGitHubApp {
		cfg, _, ok := s.githubAppConfigForTenant(context.Background(), conn.TenantID)
		if !ok {
			return nil
		}
		return forgegithub.AppRefresher{HTTP: s.forgeHTTPClient(), Cfg: cfg, Repos: s.forgeMintRepoNames}
	}
	if conn.Kind != forge.KindOAuthApp {
		return nil
	}
	app, ok := s.forgeOAuthAppFor(context.Background(), conn.TenantID, conn.Provider, conn.BaseURL())
	if !ok {
		return nil
	}
	r, _ := app.(forge.TokenRefresher)
	return r
}

// ---- handlers ----

func (s *Server) handleListForgeConnections(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	list, err := s.forgeConnections.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []forge.Connection{}
	}
	writeJSON(w, struct {
		Connections []forge.Connection `json:"connections"`
	}{Connections: list})
}

// forgeTeamRepo is one row of the team-wide connected-repo aggregator: a
// repo the team holds a RepoIntegration for, joined with its connection
// so the client renders provider + URLs without a per-connection fan-out.
type forgeTeamRepo struct {
	ConnectionID      string   `json:"connection_id"`
	ConnectionStatus  string   `json:"connection_status,omitempty"`
	Provider          string   `json:"provider"`
	RepoFullName      string   `json:"repo_full_name"`
	CloneURL          string   `json:"clone_url,omitempty"`
	WebURL            string   `json:"web_url,omitempty"`
	IntegrationID     string   `json:"integration_id"`
	BotIDs            []string `json:"bot_ids"`
	SyncIssuesEnabled bool     `json:"sync_issues_enabled"`
}

// handleListTeamForgeRepos is the RepoSwitcher's data source: the team's
// CONNECTED repos (one row per RepoIntegration) in a single call.
// Discovering not-yet-connected repos stays on the per-connection
// /connections/{conn_id}/repos picker. Absent forge stores (local mode)
// yield an empty list, not an error — the switcher just shows its CTA.
func (s *Server) handleListTeamForgeRepos(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	repos := []forgeTeamRepo{}
	if s.forgeConnections != nil && s.forgeIntegrations != nil {
		conns, err := s.forgeConnections.ListByTenant(r.Context(), teamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%s", err.Error())
			return
		}
		ints, err := s.forgeIntegrations.ListByTenant(r.Context(), teamID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%s", err.Error())
			return
		}
		byConn := make(map[string]forge.Connection, len(conns))
		for _, c := range conns {
			byConn[c.ID] = c
		}
		for _, it := range ints {
			row := forgeTeamRepo{
				ConnectionID:      it.ConnectionID,
				Provider:          string(it.Provider),
				RepoFullName:      it.RepoFullName,
				IntegrationID:     it.ID,
				BotIDs:            it.BotIDs,
				SyncIssuesEnabled: it.SyncIssuesEnabled,
			}
			if row.BotIDs == nil {
				row.BotIDs = []string{}
			}
			if c, ok := byConn[it.ConnectionID]; ok {
				row.ConnectionStatus = string(c.Status)
				row.CloneURL = forge.CloneURLFor(c.BaseURL(), it.RepoFullName)
				row.WebURL = forge.WebURLFor(c.BaseURL(), it.RepoFullName)
			}
			repos = append(repos, row)
		}
		sort.Slice(repos, func(i, j int) bool {
			if repos[i].Provider != repos[j].Provider {
				return repos[i].Provider < repos[j].Provider
			}
			return repos[i].RepoFullName < repos[j].RepoFullName
		})
	}
	writeJSON(w, struct {
		Repos []forgeTeamRepo `json:"repos"`
	}{Repos: repos})
}

// forgeConnectionHealth is the connection card's actionable state: the
// stored status/reason plus, for a GitHub App, the installation's LIVE
// repo scope and its settings URL — so the UI can explain "the repo you
// want isn't covered by the installation" and deep-link where to widen
// it, instead of dead-ending on an empty repo search.
type forgeConnectionHealth struct {
	Status               string   `json:"status"`
	StatusReason         string   `json:"status_reason,omitempty"`
	Provider             string   `json:"provider"`
	Kind                 string   `json:"kind"`
	AccountLogin         string   `json:"account_login,omitempty"`
	AppSlug              string   `json:"app_slug,omitempty"`
	InstallationID       int64    `json:"installation_id,omitempty"`
	InstallationAccount  string   `json:"installation_account,omitempty"`
	ProvisionedRepoCount int      `json:"provisioned_repo_count"`
	InstallationRepos    []string `json:"installation_repos,omitempty"`
	// ManageInstallURL is the forge-side page where the operator widens
	// the installation (repo scope + permission grants). GitHub has no
	// API for this — the link is the fix.
	ManageInstallURL string `json:"manage_install_url,omitempty"`
	// LiveError reports a failed live probe (token mint / API call)
	// without failing the endpoint — the stored status is still useful.
	LiveError string `json:"live_error,omitempty"`
}

func (s *Server) handleForgeConnectionHealth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	if s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	h := forgeConnectionHealth{
		Status:         string(conn.Status),
		StatusReason:   conn.StatusReason,
		Provider:       string(conn.Provider),
		Kind:           string(conn.Kind),
		AccountLogin:   conn.AccountLogin,
		AppSlug:        conn.AppSlug,
		InstallationID: conn.InstallationID,
	}
	if names, err := s.forgeConnRepoNames(r.Context(), conn); err == nil {
		h.ProvisionedRepoCount = len(names)
	}
	if conn.Kind == forge.KindGitHubApp && conn.InstallationID != 0 {
		if cfg, _, ok := s.githubAppConfigForTenant(r.Context(), conn.TenantID); ok {
			login, htmlURL, err := forgegithub.InstallationInfo(r.Context(), s.forgeHTTPClient(),
				forgegithub.APIBaseFor(conn.BaseURL()), cfg, conn.InstallationID, time.Now().UTC())
			if err != nil {
				h.LiveError = err.Error()
			} else {
				h.InstallationAccount = login
				h.ManageInstallURL = htmlURL
			}
		}
		if admin, err := s.forgeAdminFor(r.Context(), conn); err == nil {
			if repos, err := admin.ListRepos(r.Context(), forge.RepoQuery{}); err == nil {
				names := make([]string, 0, len(repos))
				for i, rp := range repos {
					if i >= 100 {
						break
					}
					names = append(names, rp.FullName)
				}
				h.InstallationRepos = names
			} else if h.LiveError == "" {
				h.LiveError = err.Error()
			}
		}
	}
	writeJSON(w, h)
}

type forgeCreateRepoReq struct {
	ConnectionID  string `json:"connection_id"`
	Owner         string `json:"owner,omitempty"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Private       *bool  `json:"private,omitempty"` // nil = private
	DefaultBranch string `json:"default_branch,omitempty"`
	InitReadme    bool   `json:"init_readme,omitempty"`
}

// handleCreateForgeRepo creates a NEW repository on a connected forge —
// the "new app → new repo" launch journey. Creation only: iterion never
// updates or deletes forge repositories. GitHub App connections mint a
// per-call administration:write token (see AppClient.CreateRepo); an
// installation whose grant lacks Administration surfaces the actionable
// 422 instead of silently failing.
func (s *Server) handleCreateForgeRepo(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	var req forgeCreateRepoReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || req.ConnectionID == "" {
		httpError(w, http.StatusBadRequest, "connection_id and name are required")
		return
	}
	// Tenant pin: a connection_id from another team 404s (never 403 —
	// same non-enumeration discipline as forgeConnForTenant everywhere).
	conn, ok := s.forgeConnForTenant(w, r, teamID, req.ConnectionID)
	if !ok {
		return
	}
	admin, err := s.forgeAdminFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	creator, ok := admin.(forge.RepoCreator)
	if !ok {
		httpError(w, http.StatusNotImplemented, "this connection's provider/credential cannot create repositories")
		return
	}
	private := true
	if req.Private != nil {
		private = *req.Private
	}
	sum, err := creator.CreateRepo(r.Context(), forge.RepoCreateSpec{
		Owner:         req.Owner,
		Name:          req.Name,
		Description:   req.Description,
		Private:       private,
		DefaultBranch: req.DefaultBranch,
		InitReadme:    req.InitReadme,
	})
	if err != nil {
		switch {
		case errors.Is(err, forge.ErrRepoExists):
			httpError(w, http.StatusConflict, "%v", err)
		case errors.Is(err, forge.ErrPermissionsNotGranted):
			httpError(w, http.StatusUnprocessableEntity, "the GitHub App installation lacks the Administration permission — approve the App's pending permission update on GitHub, then retry: %v", err)
		default:
			httpError(w, http.StatusBadGateway, "create repository: %v", err)
		}
		return
	}
	s.auditTenant(r, teamID, "forge.repo.created", "forge_repo", sum.FullName, map[string]any{
		"provider": string(conn.Provider), "connection_id": conn.ID, "private": private,
	})
	writeJSON(w, struct {
		Repo     forge.RepoSummary `json:"repo"`
		CloneURL string            `json:"clone_url"`
	}{Repo: sum, CloneURL: forge.CloneURLFor(conn.BaseURL(), sum.FullName)})
}

type forgeConnectReq struct {
	Provider     string `json:"provider"`
	Mode         string `json:"mode"` // "oauth" | "pat"
	ForgeBaseURL string `json:"forge_base_url,omitempty"`
	PAT          string `json:"pat,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	Next         string `json:"next,omitempty"`
}

type forgeConnectResp struct {
	Connection   *forge.Connection `json:"connection,omitempty"`
	AuthorizeURL string            `json:"authorize_url,omitempty"`
	InstallURL   string            `json:"install_url,omitempty"`
}

func (s *Server) handleConnectForge(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req forgeConnectReq
	if !decodeJSON(w, r, &req) {
		return
	}
	provider := forge.Provider(strings.TrimSpace(req.Provider))
	if !provider.Valid() {
		httpError(w, http.StatusBadRequest, "unsupported provider %q (gitlab|github|forgejo)", req.Provider)
		return
	}
	baseURL := canonicalForgeBaseURL(req.ForgeBaseURL, provider)

	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "pat":
		s.connectForgePAT(w, r, teamID, id.UserID, provider, baseURL, req)
	case "oauth", "":
		s.connectForgeOAuth(w, r, teamID, id.UserID, provider, baseURL, req)
	case "app":
		s.connectForgeGitHubApp(w, r, teamID, id.UserID, provider, req)
	default:
		httpError(w, http.StatusBadRequest, "mode must be oauth, pat, or app")
	}
}

// connectForgeGitHubApp starts the GitHub-App install flow: it returns the
// App's install URL carrying a signed state, and stashes the pending tenant
// binding so the install callback can resolve the team.
func (s *Server) connectForgeGitHubApp(w http.ResponseWriter, r *http.Request, teamID, userID string, provider forge.Provider, req forgeConnectReq) {
	if provider != forge.ProviderGitHub {
		httpError(w, http.StatusBadRequest, "the app mode is GitHub-only")
		return
	}
	cfg, _, ok := s.githubAppConfigForTenant(r.Context(), teamID)
	if !ok {
		httpError(w, http.StatusBadRequest, "no GitHub App available — first create one (Register an OAuth app → Create a GitHub App), or use OAuth/PAT")
		return
	}
	state, _, _, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	binding, err := newAgentBindingToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.forgeStates.put(forgePending{
		State: state, Provider: forge.ProviderGitHub, ForgeBaseURL: forge.DefaultBaseURL(forge.ProviderGitHub),
		TenantID: teamID, UserID: userID, AgentBinding: binding,
		NextURL: safeNext(req.Next), IssuedAt: time.Now().UTC(),
	})
	s.setForgeAgentBindingCookie(w, binding)
	installURL := "https://github.com/apps/" + url.PathEscape(cfg.AppSlug) + "/installations/new?state=" + url.QueryEscape(state)
	writeJSON(w, forgeConnectResp{InstallURL: installURL})
}

func (s *Server) connectForgePAT(w http.ResponseWriter, r *http.Request, teamID, userID string, provider forge.Provider, baseURL string, req forgeConnectReq) {
	token := strings.TrimSpace(req.PAT)
	if token == "" {
		httpError(w, http.StatusBadRequest, "pat required for mode=pat")
		return
	}
	admin, err := s.forgeAdminForToken(provider, baseURL, token)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	ident, err := admin.WhoAmI(r.Context())
	if err != nil {
		if errors.Is(err, forge.ErrUnauthorized) {
			httpError(w, http.StatusBadRequest, "the token was rejected by %s — check it has api scope", provider)
			return
		}
		httpError(w, http.StatusBadGateway, "could not reach %s: %v", provider, err)
		return
	}
	connID := uuid.NewString()
	sealed, err := forge.SealPAT(s.sealer, connID, token)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal token: %v", err)
		return
	}
	now := time.Now().UTC()
	conn := forge.Connection{
		ID: connID, TenantID: teamID, Provider: provider, Kind: forge.KindPAT,
		DisplayName: strutil.FirstNonBlank(req.DisplayName, ident.Login), ForgeBaseURL: baseURL,
		AccountLogin: ident.Login, AccountID: ident.ID, Namespace: ident.Namespace,
		Status: forge.StatusActive, SealedPayload: sealed,
		CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.forgeConnections.Create(store.WithTenant(r.Context(), teamID), conn); err != nil {
		httpError(w, http.StatusInternalServerError, "persist connection: %v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.connection.created", "forge_connection", connID, map[string]any{"provider": provider, "kind": "pat"})
	conn.SealedPayload = nil // never serialise
	writeJSON(w, forgeConnectResp{Connection: &conn})
}

func (s *Server) connectForgeOAuth(w http.ResponseWriter, r *http.Request, teamID, userID string, provider forge.Provider, baseURL string, req forgeConnectReq) {
	app, ok := s.forgeOAuthAppFor(r.Context(), teamID, provider, baseURL)
	if !ok {
		httpError(w, http.StatusBadRequest, "no OAuth app is registered for %s on this instance — register one in Integrations → OAuth apps, or paste a personal access token (mode=pat)", provider)
		return
	}
	state, verifier, challenge, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	binding, err := newAgentBindingToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.forgeStates.put(forgePending{
		State: state, CodeVerifier: verifier, Provider: provider, ForgeBaseURL: baseURL,
		TenantID: teamID, UserID: userID, AgentBinding: binding,
		NextURL: safeNext(req.Next), IssuedAt: time.Now().UTC(),
	})
	s.setForgeAgentBindingCookie(w, binding)
	authURL := app.AuthorizeURL(s.forgeOAuthRedirectURI(), state, challenge, nil)
	writeJSON(w, forgeConnectResp{AuthorizeURL: authURL})
}

func (s *Server) handleForgeOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.forgeStates == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		if s.logger != nil {
			s.logger.Warn("forge oauth callback error: %s", oauthErr)
		}
		httpError(w, http.StatusBadRequest, "oauth error: %s", oauthErr)
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" {
		httpError(w, http.StatusBadRequest, "missing state or code")
		return
	}
	pending, ok := s.forgeStates.take(state)
	clearForgeAgentBindingCookie(w, s.cfg.CookieDomain, s.cfg.CookieSecure)
	if !ok {
		httpError(w, http.StatusBadRequest, "state expired or invalid")
		return
	}
	if pending.AgentBinding != "" {
		c, cerr := r.Cookie(forgeAgentBindingCookie)
		if cerr != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(pending.AgentBinding)) != 1 {
			httpError(w, http.StatusBadRequest, "agent binding mismatch")
			return
		}
	}
	app, ok := s.forgeOAuthAppFor(r.Context(), pending.TenantID, pending.Provider, pending.ForgeBaseURL)
	if !ok {
		httpError(w, http.StatusBadRequest, "oauth app no longer registered for %s", pending.Provider)
		return
	}
	tok, err := app.Exchange(r.Context(), code, s.forgeOAuthRedirectURI(), pending.CodeVerifier)
	if err != nil {
		httpError(w, http.StatusBadRequest, "token exchange failed: %v", err)
		return
	}
	admin, err := s.forgeAdminForToken(pending.Provider, pending.ForgeBaseURL, tok.AccessToken)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	ident, err := admin.WhoAmI(r.Context())
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not read identity from %s: %v", pending.Provider, err)
		return
	}
	connID := uuid.NewString()
	sealed, err := forge.SealOAuthTokens(s.sealer, connID, tok.AccessToken, tok.RefreshToken, tok.ExpiresAt)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal token: %v", err)
		return
	}
	now := time.Now().UTC()
	conn := forge.Connection{
		ID: connID, TenantID: pending.TenantID, Provider: pending.Provider, Kind: forge.KindOAuthApp,
		DisplayName: ident.Login, ForgeBaseURL: pending.ForgeBaseURL,
		AccountLogin: ident.Login, AccountID: ident.ID, Namespace: ident.Namespace,
		Status: forge.StatusActive, SealedPayload: sealed, Scopes: tok.Scopes,
		CreatedBy: pending.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if !tok.ExpiresAt.IsZero() {
		exp := tok.ExpiresAt
		conn.AccessTokenExpiresAt = &exp
	}
	if err := s.forgeConnections.Create(store.WithTenant(r.Context(), pending.TenantID), conn); err != nil {
		httpError(w, http.StatusInternalServerError, "persist connection: %v", err)
		return
	}
	s.auditTenant(r, pending.TenantID, "forge.connection.created", "forge_connection", connID, map[string]any{"provider": pending.Provider, "kind": "oauth_app"})
	target := pending.NextURL
	if target == "" {
		target = "/teams/" + pending.TenantID
	}
	http.Redirect(w, r, appendQueryParam(target, "connected", connID), http.StatusFound)
}

// handleForgeGitHubAppCallback is the GitHub-App "Setup URL" target after an
// operator installs the App. GitHub appends installation_id + state; we
// resolve the team from the signed state (not the URL), mint an initial
// installation token, seal it, and persist a github_app connection.
func (s *Server) handleForgeGitHubAppCallback(w http.ResponseWriter, r *http.Request) {
	if s.forgeStates == nil {
		httpError(w, http.StatusNotFound, "github app not enabled")
		return
	}
	state := r.URL.Query().Get("state")
	instStr := r.URL.Query().Get("installation_id")
	// GitHub redirects here with setup_action=update (and NO state) when an
	// org owner edits the installation's repo list from GitHub's own
	// settings page — outside iterion's install flow. There is nothing to
	// persist (the live scope is re-probed on demand via InstallationInfo),
	// so send the operator back to Integrations instead of a bare 400.
	if r.URL.Query().Get("setup_action") == "update" && state == "" {
		http.Redirect(w, r, "/integrations", http.StatusFound)
		return
	}
	if state == "" || instStr == "" {
		httpError(w, http.StatusBadRequest, "missing state or installation_id")
		return
	}
	pending, ok := s.forgeStates.take(state)
	clearForgeAgentBindingCookie(w, s.cfg.CookieDomain, s.cfg.CookieSecure)
	if !ok {
		httpError(w, http.StatusBadRequest, "state expired or invalid")
		return
	}
	if pending.AgentBinding != "" {
		c, cerr := r.Cookie(forgeAgentBindingCookie)
		if cerr != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(pending.AgentBinding)) != 1 {
			httpError(w, http.StatusBadRequest, "agent binding mismatch")
			return
		}
	}
	installationID, err := strconv.ParseInt(instStr, 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid installation_id")
		return
	}
	cfg, shared, ok := s.githubAppConfigForTenant(r.Context(), pending.TenantID)
	if !ok {
		httpError(w, http.StatusBadRequest, "no github app available for this org")
		return
	}
	base := forge.DefaultBaseURL(forge.ProviderGitHub)

	// Installation-ownership check (IDOR guard). installation_id is an
	// enumerable integer taken verbatim from the callback URL; the signed
	// state only proves the flow was started by this team, NOT that the team
	// owns this installation. The SHARED platform App's key can mint a token
	// for ANY installation, so without a check an attacker could substitute a
	// victim org's installation_id and capture its repos. When user-auth OAuth
	// creds are configured we verify the completing user actually has access to
	// the installation (mandatory — an attacker can't bypass by dropping the
	// code, because a missing/invalid code fails closed). Per-tenant Apps
	// (shared==false) are key-scoped and can't reach another tenant's
	// installation, so they skip this. See VerifyInstallationOwnership.
	if shared {
		if cfg.UserAuthConfigured() {
			code := r.URL.Query().Get("code")
			if verr := forgegithub.VerifyInstallationOwnership(r.Context(), s.forgeHTTPClient(), base, cfg, code, installationID); verr != nil {
				if errors.Is(verr, forgegithub.ErrInstallationNotOwned) {
					httpError(w, http.StatusForbidden, "installation is not owned by the authorizing user")
					return
				}
				httpError(w, http.StatusBadGateway, "could not verify installation ownership: %v", verr)
				return
			}
		} else if s.logger != nil {
			// Opt-in hardening not configured: the shared-App install path
			// cannot verify ownership. Log loudly so operators enable it
			// (ITERION_FORGE_GITHUB_APP_CLIENT_ID/_CLIENT_SECRET + "Request
			// user authorization during installation" on the App).
			s.logger.Warn("forge: github-app install accepted WITHOUT installation-ownership verification (shared platform app, no user-auth client creds) — set ITERION_FORGE_GITHUB_APP_CLIENT_ID/_CLIENT_SECRET to close the installation_id IDOR")
		}
	}

	now := time.Now().UTC()
	// Least-privilege: the initial connect-time token carries only iterion's
	// minimal permission set (no repositories scope yet — none provisioned;
	// the refresh worker re-scopes to the provisioned repo set thereafter).
	tok, exp, err := forgegithub.MintInstallationToken(r.Context(), s.forgeHTTPClient(), forgegithub.APIBaseFor(base), cfg, installationID, now,
		&forgegithub.InstallationTokenOptions{Permissions: forgegithub.RuntimeInstallationPermissions()})
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not mint installation token: %v", err)
		return
	}
	connID := uuid.NewString()
	// Seal the installation token like an OAuth access token (no refresh
	// token — the refresh worker re-mints from the App private key).
	sealed, err := forge.SealOAuthTokens(s.sealer, connID, tok, "", exp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal token: %v", err)
		return
	}
	conn := forge.Connection{
		ID: connID, TenantID: pending.TenantID, Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		DisplayName: cfg.AppSlug, ForgeBaseURL: base,
		AccountLogin: cfg.AppSlug + "[bot]", Namespace: cfg.AppSlug,
		InstallationID: installationID, AppSlug: cfg.AppSlug,
		Status: forge.StatusActive, SealedPayload: sealed, AccessTokenExpiresAt: &exp,
		CreatedBy: pending.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.forgeConnections.Create(store.WithTenant(r.Context(), pending.TenantID), conn); err != nil {
		httpError(w, http.StatusInternalServerError, "persist connection: %v", err)
		return
	}
	s.auditTenant(r, pending.TenantID, "forge.connection.created", "forge_connection", connID, map[string]any{"provider": "github", "kind": "github_app"})
	target := pending.NextURL
	if target == "" {
		target = "/teams/" + pending.TenantID
	}
	http.Redirect(w, r, appendQueryParam(target, "connected", connID), http.StatusFound)
}

func (s *Server) handleDeleteForgeConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	connID := r.PathValue("conn_id")
	ctx := store.WithTenant(r.Context(), teamID)
	if err := s.forgeOrchestrator.DeprovisionConnection(ctx, teamID, connID); err != nil {
		if errors.Is(err, forge.ErrConnectionNotFound) {
			httpError(w, http.StatusNotFound, "connection not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "disconnect failed: %v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.connection.deleted", "forge_connection", connID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListForgeRepos(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	admin, err := s.forgeAdminFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	repos, err := admin.ListRepos(r.Context(), forge.RepoQuery{
		Search: r.URL.Query().Get("search"),
		Page:   page,
	})
	if err != nil {
		if errors.Is(err, forge.ErrUnauthorized) {
			httpError(w, http.StatusBadRequest, "connection credential rejected — reconnect")
			return
		}
		httpError(w, http.StatusBadGateway, "list repos: %v", err)
		return
	}
	if repos == nil {
		repos = []forge.RepoSummary{}
	}
	writeJSON(w, struct {
		Repos []forge.RepoSummary `json:"repos"`
	}{Repos: repos})
}

// forgeConnForTenant fetches a connection and asserts tenant ownership.
func (s *Server) forgeConnForTenant(w http.ResponseWriter, r *http.Request, teamID, connID string) (forge.Connection, bool) {
	conn, err := s.forgeConnections.Get(r.Context(), connID)
	if err != nil || conn.TenantID != teamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return forge.Connection{}, false
	}
	return conn, true
}

// ---- helpers ----

// setForgeAgentBindingCookie issues the per-flow CSRF-binding cookie for a
// forge connect flow (the OAuth + GitHub-App callbacks verify it; the PAT
// path has no redirect and skips it). Mirrors clearForgeAgentBindingCookie.
func (s *Server) setForgeAgentBindingCookie(w http.ResponseWriter, binding string) {
	http.SetCookie(w, &http.Cookie{
		Name:     forgeAgentBindingCookie,
		Value:    binding,
		Path:     "/api/forge/",
		Domain:   s.cfg.CookieDomain,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})
}

func clearForgeAgentBindingCookie(w http.ResponseWriter, domain string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     forgeAgentBindingCookie,
		Value:    "",
		Path:     "/api/forge/",
		Domain:   domain,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// canonicalForgeBaseURL normalises an operator-supplied forge base URL to
// scheme+host (https assumed when no scheme), or returns the provider's
// canonical SaaS host when empty.
func canonicalForgeBaseURL(raw string, provider forge.Provider) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return forge.DefaultBaseURL(provider)
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	return s
}
