package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// CreateRepo implements forge.RepoCreator. Owner "" targets the token's
// own user namespace (POST /api/v1/user/repos); an org name targets
// POST /api/v1/orgs/{org}/repos.
func (c *AdminClient) CreateRepo(ctx context.Context, spec forge.RepoCreateSpec) (forge.RepoSummary, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return forge.RepoSummary{}, fmt.Errorf("forgejo: create repo: name is required")
	}
	path := "/user/repos"
	if o := strings.TrimSpace(spec.Owner); o != "" {
		path = "/orgs/" + url.PathEscape(o) + "/repos"
	}
	body := map[string]any{
		"name":        name,
		"description": spec.Description,
		"private":     spec.Private,
		"auto_init":   spec.InitReadme,
	}
	if b := strings.TrimSpace(spec.DefaultBranch); b != "" {
		body["default_branch"] = b
	}
	var out struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
	}
	code, err := c.do(ctx, http.MethodPost, path, body, &out)
	if err != nil {
		return forge.RepoSummary{}, err
	}
	switch code {
	case http.StatusCreated:
	case http.StatusConflict:
		return forge.RepoSummary{}, fmt.Errorf("%w: forgejo POST %s returned 409 (name %q taken)", forge.ErrRepoExists, path, name)
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
