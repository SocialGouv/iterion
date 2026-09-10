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
//
// That has a MEASURED price on the gate paths, and it is the price of least
// privilege rather than an oversight. Those paths resolve the head and then
// write or read a commit status, and the status half rides rest()'s
// management token — so where one broad token once served both calls, two
// disjoint grants now need two mints (publish, gate reconcile, gate autofix,
// /revi approve; the card's CI panel takes three, one per read profile).
// Least privilege and one mint are not simultaneously available here: what
// made it one mint was precisely the read borrowing contents/pull_requests
// WRITE it has no use for.
//
// What is missing is amortization, not a wider token: pkg/server's
// forgeAdminFor builds a fresh AppClient per request, so scopedREST's
// per-permission-set cache — and rest()'s — dies with the request and no
// token is ever reused across two. Caching the client per connection would
// collapse every count above to one mint per profile per token lifetime; it
// is deliberately NOT done here, because it needs an eviction story the
// refresh route depends on (it re-mints on purpose, to show an owner the
// grant they just approved) and a key that cannot leak a token across
// tenants.
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

// GetCIStatus takes checks AND statuses on ONE mint, so an installation short
// of either gets no CI panel at all rather than half of one. That is the
// deliberate choice, not a missing degradation: the two halves are a single
// aggregate verdict, and serving the half that answers would report the
// aggregate of a subset. Concretely — a pre-`checks` installation with a
// failing GitHub Actions check-run and a green revi/review commit status
// would render GREEN on the card, on data that omits the only failing run.
// A panel that can show a false green on a merge decision is worse than one
// that says which grant to approve, which is what the 422 does.
//
// The gap is not silent while it lasts: the connection health view lists it
// under missing_ci_permissions and `iterion remote forge connections refresh`
// prints the same line, both before anyone opens a card.
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
