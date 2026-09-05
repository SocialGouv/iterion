package github

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AppClient's half of forge.BoardClient. Every call mints a DEDICATED
// organization_projects:write installation token for that single call — the
// CreateRepo precedent — so the cached runtime token keeps its minimal
// permission set and an org-wide roadmap grant never rides an ordinary push.
//
// The pair exists at all because of the parity rule: a capability wired for
// one credential shape and not the other is a defect, and a board binding
// resolves whichever connection the team holds.

// projectsREST returns an AdminClient backed by a token minted for board calls
// only. It is deliberately NOT cached: the grant is broad, and a short-lived
// token per call bounds the blast radius of a leak to one operation.
func (a *AppClient) projectsREST(ctx context.Context) (*AdminClient, error) {
	tok, _, err := MintInstallationToken(ctx, a.HTTP, a.apiBase(), a.Cfg, a.InstallationID, a.clock(),
		&InstallationTokenOptions{Permissions: ProjectsInstallationPermissions()})
	if err != nil {
		return nil, err
	}
	return &AdminClient{HTTP: a.HTTP, APIBase: a.apiBase(), Token: tok}, nil
}

// GraphQL performs a GraphQL call under a board-scoped installation token.
func (a *AppClient) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	c, err := a.projectsREST(ctx)
	if err != nil {
		return err
	}
	return c.GraphQL(ctx, query, vars, out)
}

func (a *AppClient) GetProject(ctx context.Context, ref forge.ProjectRef) (forge.Project, error) {
	c, err := a.projectsREST(ctx)
	if err != nil {
		return forge.Project{}, err
	}
	return c.GetProject(ctx, ref)
}

func (a *AppClient) ListProjectItems(ctx context.Context, ref forge.ProjectRef, opts forge.ProjectItemListOptions) (forge.ProjectItemPage, error) {
	c, err := a.projectsREST(ctx)
	if err != nil {
		return forge.ProjectItemPage{}, err
	}
	return c.ListProjectItems(ctx, ref, opts)
}

func (a *AppClient) IssueContentID(ctx context.Context, repo string, number int) (string, error) {
	c, err := a.projectsREST(ctx)
	if err != nil {
		return "", err
	}
	return c.IssueContentID(ctx, repo, number)
}

func (a *AppClient) AddItem(ctx context.Context, projectID, contentID string) (forge.ProjectItem, error) {
	c, err := a.projectsREST(ctx)
	if err != nil {
		return forge.ProjectItem{}, err
	}
	return c.AddItem(ctx, projectID, contentID)
}

func (a *AppClient) SetSingleSelect(ctx context.Context, projectID, itemID, fieldID, optionID string) error {
	c, err := a.projectsREST(ctx)
	if err != nil {
		return err
	}
	return c.SetSingleSelect(ctx, projectID, itemID, fieldID, optionID)
}
