package github

import (
	"context"
	"encoding/json"
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
}

func (c AppConfig) Configured() bool { return c.AppID != 0 && c.PrivateKeyPEM != "" }

// MintInstallationToken trades the App JWT for a short-lived (≈1h)
// installation access token. apiBase is the REST API base (APIBaseFor).
func MintInstallationToken(ctx context.Context, httpClient *http.Client, apiBase string, cfg AppConfig, installationID int64, now time.Time) (string, time.Time, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	jwt, err := signAppJWT(cfg.AppID, cfg.PrivateKeyPEM, now)
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiBase+"/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", nil)
	if err != nil {
		return "", time.Time{}, err
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
		return "", time.Time{}, statusErr("mint installation token", resp.StatusCode)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
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
		tok, exp, err := MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock())
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
}

func (r AppRefresher) Refresh(ctx context.Context, conn forge.Connection, _ string) (forge.RefreshedToken, error) {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now()
	}
	tok, exp, err := MintInstallationToken(ctx, r.HTTP, APIBaseFor(conn.BaseURL()), r.Cfg, conn.InstallationID, now)
	if err != nil {
		return forge.RefreshedToken{}, err
	}
	return forge.RefreshedToken{AccessToken: tok, ExpiresAt: exp}, nil
}

var _ forge.Admin = (*AppClient)(nil)
var _ forge.TokenRefresher = AppRefresher{}
