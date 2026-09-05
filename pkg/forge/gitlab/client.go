// Package gitlab is the GitLab implementation of forge.Admin: the
// OUTBOUND write-side client the orchestrator uses to list a connection's
// projects and create/update/delete the iterion webhook on them. It is
// deliberately separate from pkg/webhooks/gitlab (the read-only inbound
// conversational-auth client) so the inbound path stays decoupled.
package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AdminClient talks to one GitLab instance as one connection. BaseURL is
// "https://<host>" (no /api/v4). Auth is a Bearer token — GitLab accepts
// BOTH a personal access token and an OAuth access token in the
// Authorization: Bearer header, so PAT and OAuth connections share this
// one path.
type AdminClient struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
}

// New builds an AdminClient. A nil httpClient falls back to
// http.DefaultClient.
func New(httpClient *http.Client, baseURL, token string) *AdminClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &AdminClient{HTTP: httpClient, BaseURL: strings.TrimRight(baseURL, "/"), Token: token}
}

func (c *AdminClient) Provider() forge.Provider { return forge.ProviderGitLab }

// http returns the shared adminHTTP core wired with the GitLab
// Authorization header. Built per-call so AdminClient keeps its
// struct-literal constructor surface intact for tests/callers.
func (c *AdminClient) http() forge.AdminHTTP {
	return forge.NewAdminHTTP(c.HTTP, c.BaseURL+"/api/v4", "gitlab", func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	})
}

// do performs one API call against this GitLab instance via the
// shared HTTP core. The token rides the Authorization header (never
// the URL), so it can't leak via error strings.
func (c *AdminClient) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	return c.http().Do(ctx, method, path, body, out)
}

// statusErr maps a non-2xx status to the appropriate forge sentinel.
func statusErr(op string, code int) error {
	return forge.StatusErr("gitlab", op, code)
}

// WhoAmI returns the account the token authenticates as. GitLab's
// /user returns `username` instead of `login` and a free-form `name`,
// so the response decode happens inline (the shared FetchWhoAmI on
// AdminHTTP targets the github/forgejo shape). Its `bot` flag marks the
// bot user behind a group/project access token and service accounts —
// the machine identities iterion may rebrand with the iterion-bot avatar.
func (c *AdminClient) WhoAmI(ctx context.Context) (forge.Identity, error) {
	var u struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Email    string `json:"email"`
		Name     string `json:"name"`
		Bot      bool   `json:"bot"`
	}
	code, err := c.do(ctx, http.MethodGet, "/user", nil, &u)
	if err != nil {
		return forge.Identity{}, err
	}
	if code != http.StatusOK {
		return forge.Identity{}, statusErr("GET /user", code)
	}
	kind := forge.AccountKindUser
	if u.Bot {
		kind = forge.AccountKindBot
	}
	return forge.Identity{
		Login:     u.Username,
		ID:        strconv.FormatInt(u.ID, 10),
		Email:     u.Email,
		Kind:      kind,
		Namespace: u.Username,
	}, nil
}

// SetAvatar replaces the avatar of the account the token authenticates as via
// PUT /api/v4/user/avatar (GitLab ≥ 17.0; multipart field `avatar`, at most
// 200 KiB). For a group/project access token that account is the token's bot
// user — the identity every review comment and commit status is posted under.
func (c *AdminClient) SetAvatar(ctx context.Context, png []byte) (string, error) {
	var out struct {
		AvatarURL string `json:"avatar_url"`
	}
	code, body, err := c.http().DoMultipartFile(ctx, http.MethodPut, "/user/avatar", "avatar", "iterion-bot.png", "image/png", png, &out)
	if err != nil {
		return "", err
	}
	switch code {
	case http.StatusOK:
		return out.AvatarURL, nil
	case http.StatusNotFound:
		// The route itself is absent before 17.0 — a 404 here is the instance,
		// not a missing user (statusErr would read it as a missing hook).
		return "", fmt.Errorf("%w: PUT /user/avatar needs GitLab 17.0 or newer", forge.ErrAvatarUnsupported)
	case http.StatusBadRequest:
		return "", fmt.Errorf("gitlab: avatar rejected (HTTP 400 — the file must be an image of at most 200 KiB): %s", forge.TrimBody(body))
	case http.StatusUnauthorized:
		return "", forge.ErrUnauthorized
	case http.StatusForbidden:
		return "", forge.ErrForbidden
	}
	return "", fmt.Errorf("gitlab: set avatar: HTTP %d: %s", code, forge.TrimBody(body))
}

var _ forge.AvatarSetter = (*AdminClient)(nil)

// ListRepos returns projects on which the token has at least Maintainer
// access (level 40 — the floor for managing project hooks), so the picker
// never offers a repo where CreateHook would 403.
func (c *AdminClient) ListRepos(ctx context.Context, q forge.RepoQuery) ([]forge.RepoSummary, error) {
	perPage := q.PerPage
	if perPage <= 0 || perPage > 100 {
		perPage = 50
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}
	vals := url.Values{}
	vals.Set("membership", "true")
	vals.Set("min_access_level", "40") // Maintainer — can manage hooks
	vals.Set("per_page", strconv.Itoa(perPage))
	vals.Set("page", strconv.Itoa(page))
	vals.Set("order_by", "last_activity_at")
	if s := strings.TrimSpace(q.Search); s != "" {
		vals.Set("search", s)
	}
	var projects []struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		Description       string `json:"description"`
		Visibility        string `json:"visibility"`
		DefaultBranch     string `json:"default_branch"`
		WebURL            string `json:"web_url"`
	}
	code, err := c.do(ctx, http.MethodGet, "/projects?"+vals.Encode(), nil, &projects)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET /projects", code)
	}
	out := make([]forge.RepoSummary, 0, len(projects))
	for _, p := range projects {
		out = append(out, forge.RepoSummary{
			FullName:      p.PathWithNamespace,
			Description:   p.Description,
			Private:       p.Visibility != "public",
			DefaultBranch: p.DefaultBranch,
			WebURL:        p.WebURL,
			CanAdmin:      true,
		})
	}
	return out, nil
}

// GetHook finds the iterion hook on repo by its delivery URL (idempotency
// probe). Returns (nil, nil) when no hook matches.
func (c *AdminClient) GetHook(ctx context.Context, repo, deliveryURL string) (*forge.HookHandle, error) {
	var hooks []gitlabHook
	code, err := c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/hooks", nil, &hooks)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET hooks", code)
	}
	for _, h := range hooks {
		if h.URL == deliveryURL {
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
	var hooks []gitlabHook
	code, err := c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/hooks", nil, &hooks)
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

// CreateHook registers a new project hook.
func (c *AdminClient) CreateHook(ctx context.Context, repo string, spec forge.HookSpec) (forge.HookHandle, error) {
	var h gitlabHook
	code, err := c.do(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/hooks", hookBody(spec), &h)
	if err != nil {
		return forge.HookHandle{}, err
	}
	if code/100 != 2 {
		return forge.HookHandle{}, statusErr("create hook", code)
	}
	return h.toHandle(), nil
}

// UpdateHook edits an existing project hook in place.
func (c *AdminClient) UpdateHook(ctx context.Context, repo, hookID string, spec forge.HookSpec) (forge.HookHandle, error) {
	var h gitlabHook
	code, err := c.do(ctx, http.MethodPut, "/projects/"+projectID(repo)+"/hooks/"+url.PathEscape(hookID), hookBody(spec), &h)
	if err != nil {
		return forge.HookHandle{}, err
	}
	if code/100 != 2 {
		return forge.HookHandle{}, statusErr("update hook", code)
	}
	return h.toHandle(), nil
}

// DeleteHook removes a project hook. A 404 maps to forge.ErrHookNotFound so
// the orchestrator's deprovision treats it as success.
func (c *AdminClient) DeleteHook(ctx context.Context, repo, hookID string) error {
	code, err := c.do(ctx, http.MethodDelete, "/projects/"+projectID(repo)+"/hooks/"+url.PathEscape(hookID), nil, nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("delete hook", code)
	}
	return nil
}

// projectID URL-encodes an "owner/repo" path so GitLab's
// /projects/:id-or-url-encoded-path endpoint resolves it. The slash is
// encoded too (PathEscape turns "/" into "%2F"), which is what GitLab
// expects for a namespaced path id.
func projectID(repo string) string {
	return url.PathEscape(strings.TrimSpace(repo))
}

// CreateOAuthApp registers an instance-wide OAuth application via
// POST /api/v4/applications. This endpoint requires GitLab instance-admin
// rights — a non-admin token gets 403 → forge.ErrForbidden. GitLab returns
// `application_id` (the client_id) + `secret` (the client_secret); `id` is the
// internal id used to DELETE the app later.
func (c *AdminClient) CreateOAuthApp(ctx context.Context, spec forge.OAuthAppSpec) (forge.OAuthAppCredentials, error) {
	body := map[string]any{
		"name":         spec.Name,
		"redirect_uri": spec.RedirectURI,
		"scopes":       strings.Join(spec.Scopes, " "),
		"confidential": spec.Confidential,
	}
	var out struct {
		ID            int64  `json:"id"`
		ApplicationID string `json:"application_id"`
		Secret        string `json:"secret"`
	}
	code, err := c.do(ctx, http.MethodPost, "/applications", body, &out)
	if err != nil {
		return forge.OAuthAppCredentials{}, err
	}
	if code/100 != 2 {
		return forge.OAuthAppCredentials{}, statusErr("create oauth app", code)
	}
	if out.ApplicationID == "" || out.Secret == "" {
		return forge.OAuthAppCredentials{}, fmt.Errorf("gitlab: create oauth app: empty credentials in response")
	}
	return forge.OAuthAppCredentials{
		ClientID:      out.ApplicationID,
		ClientSecret:  out.Secret,
		ProviderAppID: strconv.FormatInt(out.ID, 10),
	}, nil
}

var _ forge.OAuthAppProvisioner = (*AdminClient)(nil)
