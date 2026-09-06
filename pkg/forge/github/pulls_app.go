package github

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AppClient's half of forge.PullClient — the one an App connection resolves
// to, so a capability implemented only on *AdminClient is INVISIBLE to the
// `admin.(forge.PullClient)` the board card's PR/CI panel does. That is the
// ordinary connection shape: the studio's GitHub connect wizard creates App
// connections by default, and without this half the panel's routes
// (GET|POST /api/v1/native/issues/{id}/pulls, .../pulls/{n}/ci,
// .../pulls/{n}/merge) answered 501 while a PAT connection served them.
//
// Each method names the permission profile its own call is gated on and
// reaches it through the one scopedREST chokepoint (minting + per-set
// caching). None rides the cached management token, whose write grants no
// read here has a use for; a profile the installation never approved is
// refused at the mint and reported as a *forge.PermissionError naming the
// grant — not as an opaque 422 or a silent 403.
var _ forge.PullClient = (*AppClient)(nil)

func (a *AppClient) ListPullRequests(ctx context.Context, repo string, opts forge.PullListOptions) ([]forge.PullRef, error) {
	c, err := a.scopedREST(ctx, PullListInstallationPermissions())
	if err != nil {
		return nil, err
	}
	return c.ListPullRequests(ctx, repo, opts)
}

// GetPullRequest also anchors the merge gate (the head SHA its commit status
// is posted on) and every PR-resolving webhook lane; it reads under the get
// profile, not the management token.
func (a *AppClient) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	c, err := a.scopedREST(ctx, PullGetInstallationPermissions())
	if err != nil {
		return forge.PullRef{}, err
	}
	return c.GetPullRequest(ctx, repo, number)
}

func (a *AppClient) CreatePull(ctx context.Context, repo string, in forge.NewPull) (forge.PullRef, error) {
	c, err := a.scopedREST(ctx, PullWriteInstallationPermissions())
	if err != nil {
		return forge.PullRef{}, err
	}
	return c.CreatePull(ctx, repo, in)
}

func (a *AppClient) UpdatePull(ctx context.Context, repo string, number int, patch forge.PullPatch) (forge.PullRef, error) {
	c, err := a.scopedREST(ctx, PullWriteInstallationPermissions())
	if err != nil {
		return forge.PullRef{}, err
	}
	return c.UpdatePull(ctx, repo, number, patch)
}

// MergePull delegates the whole merge → re-fetch → optional branch-deletion
// sequence, so the one token carries what all three steps need.
func (a *AppClient) MergePull(ctx context.Context, repo string, number int, opts forge.MergeOptions) (forge.PullRef, error) {
	c, err := a.scopedREST(ctx, PullMergeInstallationPermissions())
	if err != nil {
		return forge.PullRef{}, err
	}
	return c.MergePull(ctx, repo, number, opts)
}

func (a *AppClient) GetCIStatus(ctx context.Context, repo, ref string) (forge.CIStatus, error) {
	c, err := a.scopedREST(ctx, CIStatusInstallationPermissions())
	if err != nil {
		return forge.CIStatus{}, err
	}
	return c.GetCIStatus(ctx, repo, ref)
}

func (a *AppClient) ListCIHistory(ctx context.Context, repo, ref string, limit int) ([]forge.CIRun, error) {
	c, err := a.scopedREST(ctx, CIHistoryInstallationPermissions())
	if err != nil {
		return nil, err
	}
	return c.ListCIHistory(ctx, repo, ref, limit)
}
