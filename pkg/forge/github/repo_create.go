package github

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"context"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// RepoAdminInstallationPermissions is the permission subset minted ONLY
// for a CreateRepo call. administration:write is a broad grant (repo
// settings), so it never rides the cached runtime token
// (RuntimeInstallationPermissions) — each create mints its own
// short-lived token and drops it.
func RepoAdminInstallationPermissions() map[string]string {
	return map[string]string{
		"administration": "write",
		"contents":       "write",
		"metadata":       "read",
	}
}

type createRepoReq struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Private     bool   `json:"private"`
	AutoInit    bool   `json:"auto_init,omitempty"`
}

type createRepoResp struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
}

// CreateRepo implements forge.RepoCreator. Owner "" targets the token's
// own user namespace (POST /user/repos); an org login targets
// POST /orgs/{owner}/repos. Spec.DefaultBranch is intentionally not
// sent: on an empty GitHub repo the FIRST PUSH decides the default
// branch (Appy's `git init -b main` flow), and with InitReadme GitHub
// applies the owner's configured default.
func (c *AdminClient) CreateRepo(ctx context.Context, spec forge.RepoCreateSpec) (forge.RepoSummary, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return forge.RepoSummary{}, fmt.Errorf("github: create repo: name is required")
	}
	path := "/user/repos"
	if o := strings.TrimSpace(spec.Owner); o != "" {
		path = "/orgs/" + url.PathEscape(o) + "/repos"
	}
	var out createRepoResp
	code, err := c.do(ctx, http.MethodPost, path, createRepoReq{
		Name:        name,
		Description: spec.Description,
		Private:     spec.Private,
		AutoInit:    spec.InitReadme,
	}, &out)
	if err != nil {
		return forge.RepoSummary{}, err
	}
	switch code {
	case http.StatusCreated:
	case http.StatusUnprocessableEntity:
		// GitHub's dominant 422 cause here is a name collision.
		return forge.RepoSummary{}, fmt.Errorf("%w: github POST %s returned 422 (name %q likely taken)", forge.ErrRepoExists, path, name)
	default:
		return forge.RepoSummary{}, statusErr("POST "+path, code)
	}
	return forge.RepoSummary{
		FullName:      out.FullName,
		Description:   out.Description,
		Private:       out.Private,
		DefaultBranch: out.DefaultBranch,
		WebURL:        out.HTMLURL,
		CanAdmin:      true,
	}, nil
}

// CreateRepo (AppClient) mints a dedicated administration:write
// installation token for this single call — the cached runtime token
// keeps its minimal permission set. Requires the installation to have
// the Administration permission granted; an ungranted install surfaces
// forge.ErrPermissionsNotGranted with GitHub's actionable message
// (fix: approve the App's pending permission update on GitHub).
func (a *AppClient) CreateRepo(ctx context.Context, spec forge.RepoCreateSpec) (forge.RepoSummary, error) {
	tok, _, err := MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock(),
		&InstallationTokenOptions{Permissions: RepoAdminInstallationPermissions()})
	if err != nil {
		return forge.RepoSummary{}, err
	}
	c := &AdminClient{HTTP: a.HTTP, APIBase: a.apiBase(), Token: tok}
	return c.CreateRepo(ctx, spec)
}
