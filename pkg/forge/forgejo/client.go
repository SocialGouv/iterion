// Package forgejo is the Forgejo/Gitea implementation of forge.Admin. Its
// REST shape mirrors GitHub (an `events` array + a nested `config`) but the
// API lives under /api/v1 and auth is the Gitea `token` scheme.
package forgejo

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AdminClient talks to one Forgejo/Gitea instance as one connection.
// BaseURL is the web base ("https://codeberg.org" or self-hosted). Auth is
// the Gitea token header; OAuth access tokens also authenticate this way on
// current Forgejo/Gitea.
type AdminClient struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
}

func New(httpClient *http.Client, baseURL, token string) *AdminClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &AdminClient{HTTP: httpClient, BaseURL: strings.TrimRight(baseURL, "/"), Token: token}
}

func (c *AdminClient) Provider() forge.Provider { return forge.ProviderForgejo }

// http returns the shared adminHTTP core wired with the Gitea
// `token` Authorization header. Built per-call so AdminClient keeps
// its struct-literal constructor surface intact.
func (c *AdminClient) http() forge.AdminHTTP {
	return forge.NewAdminHTTP(c.HTTP, c.BaseURL+"/api/v1", "forgejo", func(req *http.Request) {
		req.Header.Set("Authorization", "token "+c.Token)
		req.Header.Set("Accept", "application/json")
	})
}

func (c *AdminClient) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	return c.http().Do(ctx, method, path, body, out)
}

func statusErr(op string, code int) error {
	return forge.StatusErr("forgejo", op, code)
}

func (c *AdminClient) WhoAmI(ctx context.Context) (forge.Identity, error) {
	return c.http().FetchWhoAmI(ctx, "/user")
}

// CollaboratorPermission returns user's permission on repo ("owner/repo") via
// GET /repos/{repo}/collaborators/{user}/permission — one of
// owner|admin|write|read|none on Forgejo/Gitea ("owner" for the repository
// owner). A 404 (not a collaborator) is
// "none", not an error. Used by the inbound-webhook command gate to authorize
// a commenter against a bot's MinReplierRole.
func (c *AdminClient) CollaboratorPermission(ctx context.Context, repo, user string) (string, error) {
	var resp struct {
		Permission string `json:"permission"` // admin|write|read|none
	}
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/collaborators/"+url.PathEscape(user)+"/permission", nil, &resp)
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "none", nil
	}
	if code != http.StatusOK {
		return "", statusErr("GET collaborator permission", code)
	}
	if resp.Permission == "" {
		return "none", nil
	}
	return resp.Permission, nil
}

// ListRepos returns repos the token can admin. Gitea's /user/repos returns
// a `permissions.admin` flag the picker filters on.
func (c *AdminClient) ListRepos(ctx context.Context, q forge.RepoQuery) ([]forge.RepoSummary, error) {
	perPage := q.PerPage
	if perPage <= 0 || perPage > 50 {
		perPage = 50
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	vals := url.Values{}
	vals.Set("limit", strconv.Itoa(perPage))
	vals.Set("page", strconv.Itoa(page))
	var repos []struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
		Permissions   struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	code, err := c.do(ctx, http.MethodGet, "/user/repos?"+vals.Encode(), nil, &repos)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET /user/repos", code)
	}
	needle := strings.ToLower(strings.TrimSpace(q.Search))
	out := make([]forge.RepoSummary, 0, len(repos))
	for _, r := range repos {
		if needle != "" && !strings.Contains(strings.ToLower(r.FullName), needle) {
			continue
		}
		out = append(out, forge.RepoSummary{
			FullName:      r.FullName,
			Description:   r.Description,
			Private:       r.Private,
			DefaultBranch: r.DefaultBranch,
			WebURL:        r.HTMLURL,
			CanAdmin:      r.Permissions.Admin,
		})
	}
	return out, nil
}

func (c *AdminClient) GetHook(ctx context.Context, repo, deliveryURL string) (*forge.HookHandle, error) {
	var hooks []forgejoHook
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/hooks", nil, &hooks)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET hooks", code)
	}
	for _, h := range hooks {
		if h.Config.URL == deliveryURL {
			hh := h.toHandle()
			return &hh, nil
		}
	}
	return nil, nil
}

// ListHooks returns every webhook registered on repo (not just the
// iterion-owned one), for operator audit. Mirrors GetHook's listing but
// without the delivery-URL filter.
func (c *AdminClient) ListHooks(ctx context.Context, repo string) ([]forge.HookHandle, error) {
	var hooks []forgejoHook
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/hooks", nil, &hooks)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET hooks", code)
	}
	out := make([]forge.HookHandle, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, h.toHandle())
	}
	return out, nil
}

func (c *AdminClient) CreateHook(ctx context.Context, repo string, spec forge.HookSpec) (forge.HookHandle, error) {
	var h forgejoHook
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/hooks", hookBody(spec), &h)
	if err != nil {
		return forge.HookHandle{}, err
	}
	if code/100 != 2 {
		return forge.HookHandle{}, statusErr("create hook", code)
	}
	return h.toHandle(), nil
}

func (c *AdminClient) UpdateHook(ctx context.Context, repo, hookID string, spec forge.HookSpec) (forge.HookHandle, error) {
	var h forgejoHook
	code, err := c.do(ctx, http.MethodPatch, "/repos/"+repo+"/hooks/"+url.PathEscape(hookID), editBody(spec), &h)
	if err != nil {
		return forge.HookHandle{}, err
	}
	if code/100 != 2 {
		return forge.HookHandle{}, statusErr("update hook", code)
	}
	if h.ID == 0 { // some Gitea versions return 200 with empty body on edit
		hid, _ := strconv.ParseInt(hookID, 10, 64)
		h.ID = hid
		h.Config.URL = spec.URL
		h.Events = spec.Events
	}
	return h.toHandle(), nil
}

func (c *AdminClient) DeleteHook(ctx context.Context, repo, hookID string) error {
	code, err := c.do(ctx, http.MethodDelete, "/repos/"+repo+"/hooks/"+url.PathEscape(hookID), nil, nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("delete hook", code)
	}
	return nil
}

// SetAvatar replaces the avatar of the token's account via
// POST /api/v1/user/avatar (Gitea ≥ 1.20, inherited by Forgejo): a JSON body
// carrying the raw image bytes base64-encoded, 204 on success. Forgejo reports
// no URL back, so the returned one is empty. Forgejo has no bot flag on users,
// so this is only ever reached through the operator's explicit apply action.
func (c *AdminClient) SetAvatar(ctx context.Context, png []byte) (string, error) {
	body := map[string]any{"image": base64.StdEncoding.EncodeToString(png)}
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodPost, "/user/avatar", body, nil)
	if err != nil {
		return "", err
	}
	switch {
	case code/100 == 2:
		return "", nil
	case code == http.StatusNotFound:
		// The route itself is absent on older releases (statusErr would read a
		// 404 as a missing hook).
		return "", fmt.Errorf("%w: POST /user/avatar answered 404 — Gitea/Forgejo older than 1.20, or a base URL that redirects: %s",
			forge.ErrAvatarUnsupported, forge.TrimBody(errBody))
	case code == http.StatusUnauthorized:
		return "", forge.ErrUnauthorized
	case code == http.StatusForbidden:
		return "", forge.ErrForbidden
	}
	// The refusal keeps its reason (a lowered AVATAR_MAX_FILE_SIZE answers
	// 413 with the limit in the body) — it lands on the connection record.
	return "", fmt.Errorf("forgejo: set avatar: HTTP %d: %s", code, forge.TrimBody(errBody))
}

var _ forge.AvatarSetter = (*AdminClient)(nil)

// CreateOAuthApp registers a user-owned OAuth2 application via
// POST /api/v1/user/applications/oauth2 — a normal authenticated user can do
// this (no admin needed). Gitea/Forgejo attaches scopes at authorize time, not
// at creation, so spec.Scopes is not sent here. Returns client_id +
// client_secret and the numeric app id.
func (c *AdminClient) CreateOAuthApp(ctx context.Context, spec forge.OAuthAppSpec) (forge.OAuthAppCredentials, error) {
	body := map[string]any{
		"name":                spec.Name,
		"redirect_uris":       []string{spec.RedirectURI},
		"confidential_client": spec.Confidential,
	}
	var out struct {
		ID           int64  `json:"id"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	code, err := c.do(ctx, http.MethodPost, "/user/applications/oauth2", body, &out)
	if err != nil {
		return forge.OAuthAppCredentials{}, err
	}
	if code/100 != 2 {
		return forge.OAuthAppCredentials{}, statusErr("create oauth app", code)
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		return forge.OAuthAppCredentials{}, fmt.Errorf("forgejo: create oauth app: empty credentials in response")
	}
	return forge.OAuthAppCredentials{
		ClientID:      out.ClientID,
		ClientSecret:  out.ClientSecret,
		ProviderAppID: strconv.FormatInt(out.ID, 10),
	}, nil
}

var _ forge.OAuthAppProvisioner = (*AdminClient)(nil)
