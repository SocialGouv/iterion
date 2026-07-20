package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
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
		cfg, _, ok := s.githubAppConfigForConnection(ctx, conn)
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
		if app, err := s.forgeOAuthApps.GetByInstance(ctx, tenantID, forge.ProviderGitHub, base); err == nil {
			if cfg, ok := s.githubAppConfigFromRecord(app); ok {
				return cfg, false, true
			}
		}
	}
	if s.forgeGitHubApp.Configured() {
		return s.githubAppConfig(), true, true
	}
	return forgegithub.AppConfig{}, false, false
}

// githubAppConfigForConnection resolves the App identity that owns a
// connection's installation.
//
// This is the correct key. A tenant may hold several GitHub Apps on one host —
// one per owning org, since a private App is only installable on its owner — so
// the app CANNOT be re-derived from (tenant, provider, host) any more. Getting
// it wrong is not a graceful degradation: a JWT signed with app A's key cannot
// mint a token for an installation of app B, and the background refresh worker
// would keep failing (or address the wrong installation) with no obvious cause.
//
// Resolution order: the connection's own OAuthAppID, then the legacy
// per-instance lookup (unambiguous for connections created while only one app
// per host could exist), then the shared platform App.
func (s *Server) githubAppConfigForConnection(ctx context.Context, conn forge.Connection) (cfg forgegithub.AppConfig, shared bool, ok bool) {
	if conn.OAuthAppID != "" && s.forgeOAuthApps != nil {
		app, err := s.forgeOAuthApps.Get(ctx, conn.OAuthAppID)
		if err == nil && app.TenantID == conn.TenantID {
			if cfg, ok := s.githubAppConfigFromRecord(app); ok {
				return cfg, false, true
			}
		}
		// Deliberate: a connection that NAMES an app whose key is unusable must
		// not silently fall back to another tenant-level app and sign with the
		// wrong identity. Fail closed and let the caller surface it.
		if s.logger != nil {
			s.logger.Warn("forge: connection %s references oauth app %s which did not resolve to a usable key", conn.ID, conn.OAuthAppID)
		}
		return forgegithub.AppConfig{}, false, false
	}
	return s.githubAppConfigForTenant(ctx, conn.TenantID)
}

// githubAppForInstall picks the App to start an install flow with, returning
// its config and the app-record id to pin onto the pending state (and, later,
// onto the Connection). An explicit appID selects among a team's several Apps;
// empty keeps the legacy "the team's app for this host" behaviour.
//
// A cross-tenant appID resolves to nothing rather than another team's App.
// shared reports the SHARED platform App, exactly as githubAppConfigForTenant
// does — the install callback's IDOR guard keys on it, so it is returned
// explicitly rather than inferred from an empty record id (a tenant app whose
// record lookup merely failed must not be mistaken for the platform app).
func (s *Server) githubAppForInstall(ctx context.Context, tenantID, appID string) (cfg forgegithub.AppConfig, resolvedID string, shared bool, ok bool) {
	if appID != "" && s.forgeOAuthApps != nil {
		app, err := s.forgeOAuthApps.Get(ctx, appID)
		if err != nil || app.TenantID != tenantID {
			return forgegithub.AppConfig{}, "", false, false
		}
		cfg, ok := s.githubAppConfigFromRecord(app)
		if !ok {
			return forgegithub.AppConfig{}, "", false, false
		}
		return cfg, app.ID, false, true
	}
	cfg, shared, ok = s.githubAppConfigForTenant(ctx, tenantID)
	if !ok {
		return forgegithub.AppConfig{}, "", false, false
	}
	// Only a tenant-owned app has a record to pin; the shared platform app has
	// none, and pinning "" keeps those connections on the legacy resolution.
	if shared || s.forgeOAuthApps == nil {
		return cfg, "", shared, true
	}
	app, err := s.forgeOAuthApps.GetByInstance(ctx, tenantID, forge.ProviderGitHub, forge.CanonicalBaseURL(forge.ProviderGitHub, ""))
	if err != nil {
		return cfg, "", shared, true
	}
	return cfg, app.ID, shared, true
}

// githubAppConfigFromRecord unseals an app row into a usable App identity.
// Returns ok=false when the row cannot mint (no private key, unsealable, or a
// non-numeric app id) so callers treat it as "no app" rather than half-valid.
func (s *Server) githubAppConfigFromRecord(app forge.ForgeOAuthApp) (forgegithub.AppConfig, bool) {
	if len(app.SealedPrivateKey) == 0 {
		return forgegithub.AppConfig{}, false
	}
	pem, err := forge.OpenForgeAppPrivateKey(s.sealer, app.ID, app.SealedPrivateKey)
	if err != nil || pem == "" {
		return forgegithub.AppConfig{}, false
	}
	appID, _ := strconv.ParseInt(app.ProviderAppID, 10, 64)
	if appID == 0 {
		return forgegithub.AppConfig{}, false
	}
	return forgegithub.AppConfig{AppID: appID, PrivateKeyPEM: pem, AppSlug: app.AppSlug}, true
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
	cfg, _, ok := s.githubAppConfigForConnection(ctx, conn)
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

// forgeRepoReachable reports whether a github_app connection's runtime token
// will actually be able to reach repoFullName, and returns an ACTIONABLE error
// when it definitively cannot.
//
// Why this exists: a GitHub App installed on "selected repositories" mints
// tokens hard-scoped to that selection. A repo outside it — notably one iterion
// just CREATED, since GitHub does not add App-created repos to the installation
// — fails only at `git push`, with a bare 403 "denied to <app>[bot]". On a
// multi-hour app-building run that surfaces hours after launch, after all the
// work is done, and reads as an agent bug rather than a missing grant.
//
// Best-effort by construction: only a definitive negative (the API answered,
// and the repo is absent from the installation) blocks the launch. A transient
// failure to ask returns nil so a forge hiccup never blocks an otherwise valid
// run.
func (s *Server) forgeRepoReachable(ctx context.Context, conn forge.Connection, repoFullName string) error {
	if conn.Kind != forge.KindGitHubApp || repoFullName == "" {
		return nil
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return nil
	}
	repos, err := admin.ListRepos(ctx, forge.RepoQuery{})
	if err != nil {
		return nil // no answer is not a negative
	}
	return repoOutsideInstallationErr(repos, repoFullName)
}

// repoOutsideInstallationErr is forgeRepoReachable's decision, split out so the
// rule is testable without an Admin seam. An EMPTY list is treated as "no
// usable answer", not as "absent": an installation we cannot enumerate must
// never block a launch.
func repoOutsideInstallationErr(repos []forge.RepoSummary, repoFullName string) error {
	if len(repos) == 0 || repoFullName == "" {
		return nil
	}
	want := shortRepoName(repoFullName)
	for _, r := range repos {
		if shortRepoName(r.FullName) == want {
			return nil
		}
	}
	return fmt.Errorf(
		"the GitHub App installation cannot reach %s — its token is scoped to the installation's selected repositories, so any push would fail with 403. Add the repository to the installation (GitHub → the App → Configure → Repository access), or install the App on \"All repositories\" in a namespace you own",
		repoFullName)
}

// forgeRefresherFor returns the token refresher for a connection, or nil
// when it cannot/should-not refresh (PAT, GitHub-App, or a provider with no
// configured OAuth app). The per-provider OAuth clients implement both
// OAuthExchanger and TokenRefresher.
func (s *Server) forgeRefresherFor(conn forge.Connection) forge.TokenRefresher {
	if conn.Kind == forge.KindGitHubApp {
		cfg, _, ok := s.githubAppConfigForConnection(context.Background(), conn)
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
