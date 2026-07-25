package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// CreateRepo implements forge.RepoCreator via POST /api/v4/projects.
// Owner "" targets the token's own user namespace; a group full path is
// resolved to its namespace_id first (GET /api/v4/groups/{path}).
func (c *AdminClient) CreateRepo(ctx context.Context, spec forge.RepoCreateSpec) (forge.RepoSummary, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return forge.RepoSummary{}, fmt.Errorf("gitlab: create repo: name is required")
	}
	body := map[string]any{
		"name":                   name,
		"description":            spec.Description,
		"initialize_with_readme": spec.InitReadme,
	}
	if spec.Private {
		body["visibility"] = "private"
	} else {
		body["visibility"] = "public"
	}
	if b := strings.TrimSpace(spec.DefaultBranch); b != "" {
		body["default_branch"] = b
	}
	if owner := strings.TrimSpace(spec.Owner); owner != "" {
		var grp struct {
			ID int64 `json:"id"`
		}
		code, err := c.do(ctx, http.MethodGet, "/groups/"+url.PathEscape(owner), nil, &grp)
		if err != nil {
			return forge.RepoSummary{}, err
		}
		if code != http.StatusOK {
			return forge.RepoSummary{}, statusErr("GET /groups/"+owner, code)
		}
		body["namespace_id"] = grp.ID
	}
	var out struct {
		PathWithNamespace string `json:"path_with_namespace"`
		Description       string `json:"description"`
		Visibility        string `json:"visibility"`
		DefaultBranch     string `json:"default_branch"`
		WebURL            string `json:"web_url"`
	}
	code, err := c.do(ctx, http.MethodPost, "/projects", body, &out)
	if err != nil {
		return forge.RepoSummary{}, err
	}
	switch code {
	case http.StatusCreated:
	case http.StatusBadRequest, http.StatusConflict:
		// GitLab reports a name collision as a 400 "has already been taken".
		return forge.RepoSummary{}, fmt.Errorf("%w: gitlab POST /projects returned %d (name %q likely taken)", forge.ErrRepoExists, code, name)
	default:
		return forge.RepoSummary{}, statusErr("POST /projects", code)
	}
	return forge.RepoSummary{
		FullName:      out.PathWithNamespace,
		Description:   out.Description,
		Private:       out.Visibility != "public",
		DefaultBranch: out.DefaultBranch,
		WebURL:        out.WebURL,
		CanAdmin:      true,
	}, nil
}
