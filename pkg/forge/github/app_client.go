package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AppConfig is the global GitHub-App identity (registered once on GitHub),
// shared across every installation. The private key never leaves the
// process; it is loaded from deployment config, not from Mongo.
type AppConfig struct {
	AppID         int64
	PrivateKeyPEM string
	AppSlug       string // for the install URL github.com/apps/<slug>/installations/new
	// ClientID/ClientSecret are the App's user-authorization OAuth
	// credentials. Optional: when both are set (and the App has "Request
	// user authorization (OAuth) during installation" enabled on GitHub),
	// the install callback verifies the completing user actually owns the
	// installation before minting a token for it — see
	// VerifyInstallationOwnership. Empty → verification is unavailable.
	ClientID     string
	ClientSecret string
}

func (c AppConfig) Configured() bool { return c.AppID != 0 && c.PrivateKeyPEM != "" }

// UserAuthConfigured reports whether the App carries the OAuth client
// credentials needed to verify installation ownership at connect time.
func (c AppConfig) UserAuthConfigured() bool { return c.ClientID != "" && c.ClientSecret != "" }

// RuntimeInstallationPermissions is the least-privilege permission subset an
// installation token minted for iterion is pinned to — exactly what the forge
// layer needs (read/push code, open+comment PRs, comment the source issue for
// the MR back-link, manage the per-repo webhook, the mandatory metadata
// baseline) and nothing more. Mirrors the App manifest (BuildAppManifest);
// pinning it at mint time means a token stays minimal even if the installation
// is later granted broader permissions on the forge.
func RuntimeInstallationPermissions() map[string]string {
	return map[string]string{
		"contents":         "write",
		"pull_requests":    "write",
		"issues":           "write", // finalize_mr posts the PR URL back on the source issue
		"metadata":         "read",
		"repository_hooks": "write",
	}
}

// DeliveryInstallationPermissions are what a bot needs to SHIP an application
// rather than only change code: publish the CI definition that builds it, and
// publish the resulting image.
//
// They are separated from the runtime baseline because `workflows: write` is a
// genuine escalation — an actor who can rewrite CI can run arbitrary code in
// it — so an App only gets them when the operator opts in at creation.
//
// GitHub enforces this hard, not softly: pushing a commit that touches
// .github/workflows/** with a token lacking `workflows` is REJECTED outright
// ("refusing to allow a GitHub App to create or update workflow … without
// `workflows` permission"), which blocks the whole build-and-deploy chain at
// its first step.
func DeliveryInstallationPermissions() map[string]string {
	return map[string]string{
		"workflows": "write",
		"packages":  "write",
	}
}

// MissingDeliveryPermissions lists the delivery grants an installation does
// NOT have, so the connection health view can name them BEFORE a run spends
// hours discovering them at push time. Empty when nothing is missing (or when
// the grant set is unknown — absence of data is not evidence of a gap).
func MissingDeliveryPermissions(granted map[string]string) []string {
	if len(granted) == 0 {
		return nil
	}
	var missing []string
	for _, name := range []string{"packages", "workflows"} { // sorted: stable output
		if _, ok := granted[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// RuntimePermissionsFor narrows the permissions iterion WANTS to those the
// installation actually granted.
//
// Minting is not forgiving: asking for a permission the installation lacks
// fails the whole call (422), so a token cannot simply request the superset.
// Intersecting keeps one code path for both an App created before delivery
// permissions existed and one created with them.
//
// granted == nil means "unknown" (a connection stored before grants were
// recorded); those keep exactly the historical baseline.
func RuntimePermissionsFor(granted map[string]string) map[string]string {
	base := RuntimeInstallationPermissions()
	if len(granted) == 0 {
		return base
	}
	out := map[string]string{}
	for name, level := range base {
		if _, ok := granted[name]; ok {
			out[name] = level
		}
	}
	for name, level := range DeliveryInstallationPermissions() {
		if _, ok := granted[name]; ok {
			out[name] = level
		}
	}
	// An installation that granted nothing we recognise would yield an empty
	// map, which the mint reads as "no constraint" (the installation's FULL
	// set) — the opposite of least privilege. Keep the baseline instead.
	if len(out) == 0 {
		return base
	}
	return out
}

// InstallationTokenOptions narrows a minted installation token below the
// installation's full grant (least-privilege). Both fields are optional; a nil
// field means "don't constrain that dimension" (GitHub returns the
// installation's full set).
//
//   - Repositories: short repo names (e.g. "api", NOT "org/api") the token may
//     touch. Empty → all repositories in the installation.
//   - Permissions: the permission subset (a subset of the installation's own
//     grants). Empty → the installation's full permission set.
type InstallationTokenOptions struct {
	Repositories []string
	Permissions  map[string]string
}

// Installation is what GET /app/installations/{id} tells us about a live
// installation.
type Installation struct {
	Login   string
	HTMLURL string
	// Permissions is the grant the ORG actually approved, which can be
	// narrower than what the App requests (a permission added to an App after
	// installation stays pending until an owner approves it). Carrying it lets
	// the mint ask only for what exists, and lets the health probe name a
	// missing grant BEFORE a run spends hours discovering it at push time.
	Permissions map[string]string
}

// InstallationInfo returns the installation's account login, its GitHub
// settings page URL (html_url — the only place where the installation's repo
// scope and permission grants can be widened), and its approved permissions.
// App-JWT-authenticated: GET /app/installations/{id}.
func InstallationInfo(ctx context.Context, httpClient *http.Client, apiBase string, cfg AppConfig, installationID int64, now time.Time) (info Installation, err error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	jwt, err := signAppJWT(cfg.AppID, cfg.PrivateKeyPEM, now)
	if err != nil {
		return Installation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/app/installations/"+strconv.FormatInt(installationID, 10), nil)
	if err != nil {
		return Installation{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Installation{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Installation{}, forge.ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return Installation{}, statusErr("GET /app/installations/{id}", resp.StatusCode)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Installation{}, err
	}
	return Installation{Login: out.Account.Login, HTMLURL: out.HTMLURL, Permissions: out.Permissions}, nil
}

// MintInstallationToken trades the App JWT for a short-lived (≈1h)
// installation access token. apiBase is the REST API base (APIBaseFor). opts
// may be nil for an unconstrained (whole-installation) token, or narrow it to
// specific repositories + a permission subset (least-privilege).
func MintInstallationToken(ctx context.Context, httpClient *http.Client, apiBase string, cfg AppConfig, installationID int64, now time.Time, opts *InstallationTokenOptions) (string, time.Time, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	jwt, err := signAppJWT(cfg.AppID, cfg.PrivateKeyPEM, now)
	if err != nil {
		return "", time.Time{}, err
	}
	var body io.Reader
	if opts != nil {
		payload := map[string]any{}
		if len(opts.Repositories) > 0 {
			payload["repositories"] = opts.Repositories
		}
		if len(opts.Permissions) > 0 {
			payload["permissions"] = opts.Permissions
		}
		if len(payload) > 0 {
			raw, err := json.Marshal(payload)
			if err != nil {
				return "", time.Time{}, err
			}
			body = bytes.NewReader(raw)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", body)
	if err != nil {
		return "", time.Time{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", time.Time{}, forge.ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		// GitHub's 4xx body always names the cause (e.g. a 422 "there is at
		// least one repository that does not exist or is not accessible to the
		// … installation", or an ungranted permission). Surfacing it turns an
		// opaque "HTTP 422" into an actionable message instead of masking the
		// root cause (erreurs-explicites).
		err := mintTokenErr(resp)
		// A 422 whose body reports the requested permissions are NOT GRANTED is
		// a permanent config mismatch (the install was approved with a narrower
		// permission set than iterion now requests). Classify it as the terminal
		// forge.ErrPermissionsNotGranted so the refresh worker marks the
		// connection degraded and stops re-minting it every tick, while keeping
		// GitHub's own actionable message in the wrapped error.
		if resp.StatusCode == http.StatusUnprocessableEntity && isPermissionsNotGranted(err) {
			return "", time.Time{}, fmt.Errorf("%w: %w", forge.ErrPermissionsNotGranted, err)
		}
		return "", time.Time{}, err
	}
	var out struct {
		Token     string            `json:"token"`
		ExpiresAt string            `json:"expires_at"`
		Perms     map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	exp, _ := time.Parse(time.RFC3339, out.ExpiresAt)
	if exp.IsZero() {
		exp = now.Add(time.Hour)
	}
	return out.Token, exp, nil
}

// lastMintedPermissions remembers what the most recent RUNTIME token for an
// installation actually CARRIED, keyed by installation id.
//
// It exists because checking the installation's grant is necessary but NOT
// sufficient, and mistaking one for the other cost a full run: the
// installation had `workflows`, the minted token did not (it was pinned to the
// baseline), and a pre-flight that read the grant reported all-clear while the
// push was refused an hour later. The token is the thing that acts, so the
// token is the thing to inspect.
//
// Only the RUNTIME token is recorded — the one sealed into a run and used by
// the bot. AppClient.rest mints a separate MANAGEMENT token, deliberately
// pinned to the baseline for iterion's own API calls; recording that one would
// make the pre-flight report a narrow token for a run that will get a wide one,
// which is the same wrong-layer mistake in the opposite direction.
var lastMintedPermissions sync.Map // installationID(int64) → map[string]string

// RecordRuntimePermissions notes what a runtime token carried. Called by the
// mint paths that produce the token a RUN uses.
func RecordRuntimePermissions(installationID int64, perms map[string]string) {
	if len(perms) > 0 {
		lastMintedPermissions.Store(installationID, perms)
	}
}

// LastMintedPermissions returns the permissions carried by the most recent
// runtime token minted for an installation in this process, and whether one is
// known.
func LastMintedPermissions(installationID int64) (map[string]string, bool) {
	v, ok := lastMintedPermissions.Load(installationID)
	if !ok {
		return nil, false
	}
	perms, _ := v.(map[string]string)
	return perms, len(perms) > 0
}

// mintTokenErr wraps the base status error with GitHub's own explanation from
// the response body (message + first field error), so a 422 says WHY. The body
// is a short GitHub error JSON; a decode failure falls back to the bare status
// error. Never includes any token (this is the pre-auth mint call).
func mintTokenErr(resp *http.Response) error {
	base := statusErr("mint installation token", resp.StatusCode)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || len(raw) == 0 {
		return base
	}
	var ghErr struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(raw, &ghErr) != nil || ghErr.Message == "" {
		return base
	}
	detail := ghErr.Message
	if len(ghErr.Errors) > 0 {
		e := ghErr.Errors[0]
		if msg := strings.TrimSpace(e.Message); msg != "" {
			detail += ": " + msg
		} else if e.Field != "" || e.Code != "" {
			detail += " (" + strings.TrimSpace(e.Field+" "+e.Code) + ")"
		}
	}
	return fmt.Errorf("%w: %s", base, detail)
}

// isPermissionsNotGranted reports whether a mint error's message matches
// GitHub's "The permissions requested are not granted to this installation"
// 422 body — the permanent permission-mismatch signal (distinct from the
// "repository does not exist" 422). Matched on the already-surfaced message so
// it survives mintTokenErr's message/errors[] extraction.
func isPermissionsNotGranted(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permissions") && strings.Contains(msg, "not granted")
}

// AppClient is a forge.Admin for one GitHub-App installation. It mints +
// caches the installation token (refreshing ≈60s before expiry) and
// delegates the actual REST calls to an AdminClient. Repo listing + identity
// differ from a user token (an installation token can't read /user), so
// those are overridden.
type AppClient struct {
	HTTP           *http.Client
	WebBaseURL     string
	Cfg            AppConfig
	InstallationID int64
	Now            func() time.Time

	mu    sync.Mutex
	token string
	exp   time.Time
}

func (a *AppClient) clock() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now().UTC()
}

func (a *AppClient) apiBase() string { return APIBaseFor(a.WebBaseURL) }

// rest returns an AdminClient backed by a fresh installation token.
func (a *AppClient) rest(ctx context.Context) (*AdminClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == "" || a.clock().After(a.exp.Add(-60*time.Second)) {
		// Least-privilege: pin the management token to iterion's minimal
		// permission set (webhook + metadata + code + PR), never the
		// installation's full grant — plus the OPTIONAL statuses:write the
		// merge gate needs to post its revi/review commit status.
		perms := map[string]string{"statuses": "write"}
		for k, v := range RuntimeInstallationPermissions() {
			perms[k] = v
		}
		tok, exp, err := MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock(),
			&InstallationTokenOptions{Permissions: perms})
		if errors.Is(err, forge.ErrPermissionsNotGranted) {
			// An installation created before the merge gate (or one that
			// declined statuses:write) still works — the gate then advises
			// instead of blocking (SetCommitStatus 403s, non-fatal). Retry with
			// the core baseline so every other capability keeps functioning.
			tok, exp, err = MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock(),
				&InstallationTokenOptions{Permissions: RuntimeInstallationPermissions()})
		}
		if err != nil {
			return nil, err
		}
		a.token, a.exp = tok, exp
	}
	return &AdminClient{HTTP: a.HTTP, APIBase: a.apiBase(), Token: a.token}, nil
}

func (a *AppClient) Provider() forge.Provider { return forge.ProviderGitHub }

// WhoAmI returns the App identity — an installation token can't call /user,
// and the bot posts AS the App, so this is the correct "post as" handle.
func (a *AppClient) WhoAmI(context.Context) (forge.Identity, error) {
	slug := a.Cfg.AppSlug
	if slug == "" {
		slug = "github-app"
	}
	return forge.Identity{Login: slug + "[bot]", ID: strconv.FormatInt(a.Cfg.AppID, 10), Kind: "bot", Namespace: slug}, nil
}

// ListRepos lists the installation's repositories (GET
// /installation/repositories) — an installation token's repo set, not the
// user's. The App was installed with webhook-write permission, so every
// listed repo is admin-capable (a missing permission surfaces as a 403 on
// CreateHook, mapped to insufficient_scope).
func (a *AppClient) ListRepos(ctx context.Context, q forge.RepoQuery) ([]forge.RepoSummary, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Repositories []struct {
			FullName      string `json:"full_name"`
			Description   string `json:"description"`
			Private       bool   `json:"private"`
			DefaultBranch string `json:"default_branch"`
			HTMLURL       string `json:"html_url"`
		} `json:"repositories"`
	}
	code, err := c.do(ctx, http.MethodGet, "/installation/repositories?per_page=100", nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET /installation/repositories", code)
	}
	needle := strings.ToLower(strings.TrimSpace(q.Search))
	repos := make([]forge.RepoSummary, 0, len(out.Repositories))
	for _, r := range out.Repositories {
		if needle != "" && !strings.Contains(strings.ToLower(r.FullName), needle) {
			continue
		}
		repos = append(repos, forge.RepoSummary{
			FullName: r.FullName, Description: r.Description, Private: r.Private,
			DefaultBranch: r.DefaultBranch, WebURL: r.HTMLURL, CanAdmin: true,
		})
	}
	return repos, nil
}

func (a *AppClient) GetHook(ctx context.Context, repo, deliveryURL string) (*forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetHook(ctx, repo, deliveryURL)
}

func (a *AppClient) CreateHook(ctx context.Context, repo string, spec forge.HookSpec) (forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return forge.HookHandle{}, err
	}
	return c.CreateHook(ctx, repo, spec)
}

func (a *AppClient) UpdateHook(ctx context.Context, repo, hookID string, spec forge.HookSpec) (forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return forge.HookHandle{}, err
	}
	return c.UpdateHook(ctx, repo, hookID, spec)
}

func (a *AppClient) DeleteHook(ctx context.Context, repo, hookID string) error {
	c, err := a.rest(ctx)
	if err != nil {
		return err
	}
	return c.DeleteHook(ctx, repo, hookID)
}

func (a *AppClient) ListHooks(ctx context.Context, repo string) ([]forge.HookHandle, error) {
	c, err := a.rest(ctx)
	if err != nil {
		return nil, err
	}
	return c.ListHooks(ctx, repo)
}

// AppRefresher re-mints the installation token for the connection's managed
// forge_token secret (forge.TokenRefresher). The refreshToken arg is unused
// — a GitHub App re-mints from its private key, not a refresh token.
type AppRefresher struct {
	HTTP *http.Client
	Cfg  AppConfig
	Now  func() time.Time
	// Repos, when set, returns the short repo names (e.g. "api", not
	// "org/api") this connection actually operates on, so the runtime
	// forge_token is scoped to that repo set (least-privilege) instead of the
	// whole installation. A nil slice with a nil error → whole-installation
	// (still minimal permissions). A non-nil error means the set could not be
	// determined; Refresh fails closed rather than minting a broader token.
	// Injected by the server so the refresher stays free of a store dependency.
	Repos func(ctx context.Context, conn forge.Connection) ([]string, error)
}

func (r AppRefresher) Refresh(ctx context.Context, conn forge.Connection, _ string) (forge.RefreshedToken, error) {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now()
	}
	// Least-privilege: the runtime forge token carries only iterion's minimal
	// permission set, scoped to the connection's provisioned repos when known.
	opts := &InstallationTokenOptions{Permissions: RuntimePermissionsFor(conn.GrantedPermissions)}
	if r.Repos != nil {
		repos, err := r.Repos(ctx, conn)
		if err != nil {
			// Fail closed: keep the prior (narrower) token by failing the
			// refresh rather than minting a whole-installation token when the
			// provisioned repo set is momentarily unknown.
			return forge.RefreshedToken{}, err
		}
		opts.Repositories = repos
	}
	tok, exp, err := MintInstallationToken(ctx, r.HTTP, APIBaseFor(conn.BaseURL()), r.Cfg, conn.InstallationID, now, opts)
	if err != nil {
		return forge.RefreshedToken{}, err
	}
	RecordRuntimePermissions(conn.InstallationID, opts.Permissions)
	return forge.RefreshedToken{AccessToken: tok, ExpiresAt: exp}, nil
}

var _ forge.Admin = (*AppClient)(nil)
var _ forge.TokenRefresher = AppRefresher{}
