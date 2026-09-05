package github

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AppClient's half of forge.BoardClient. Every call goes through a DEDICATED
// organization_projects:write installation token, cached separately from the
// runtime one, so the runtime token keeps its minimal permission set and an
// org-wide roadmap grant never rides an ordinary push.
//
// The pair exists at all because of the parity rule: a capability wired for
// one credential shape and not the other is a defect, and a board binding
// resolves whichever connection the team holds.

// projectsREST returns an AdminClient backed by a token minted for board calls
// only, cached per permission set (scopedREST): the org-wide grant is served
// back only to another board call, never to an ordinary push, while one
// reconciliation pass pays ONE mint instead of one per project read, item page
// and reflected card.
func (a *AppClient) projectsREST(ctx context.Context) (*AdminClient, error) {
	return a.scopedREST(ctx, ProjectsInstallationPermissions())
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

func (a *AppClient) ItemForIssue(ctx context.Context, ref forge.ProjectRef, repo string, number int) (forge.ProjectItem, bool, error) {
	c, err := a.projectsREST(ctx)
	if err != nil {
		return forge.ProjectItem{}, false, err
	}
	return c.ItemForIssue(ctx, ref, repo, number)
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
