package github

import (
	"context"
	"net/http"
	"net/url"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// SetCommitStatus posts a commit status onto sha via the GitHub commit-status
// API (POST /repos/{repo}/statuses/{sha}). The status `context` is the check
// name branch protection matches on; a required-check ruleset listing it makes
// the merge queue block until it is green. Needs the `statuses: write` (or
// legacy `repo:status`) scope on the connection.
func (c *AdminClient) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	body := map[string]string{
		"state":       githubCommitState(st.State),
		"context":     st.Context,
		"description": forge.TruncateStatusDescription(st.Description),
	}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	var out struct {
		ID int64 `json:"id"`
	}
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/statuses/"+url.PathEscape(sha), body, &out)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("set commit status", code)
	}
	return nil
}

// SetCommitStatus on an App connection delegates to a management-token
// AdminClient minted on demand (same live-token rationale as CreatePullReview)
// — so the merge gate works on the production GitHub App path, not only on
// raw-token connections.
func (a *AppClient) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	rest, err := a.rest(ctx)
	if err != nil {
		return err
	}
	return rest.SetCommitStatus(ctx, repo, sha, st)
}

// GetPullRequest on an App connection delegates to a fresh-token AdminClient —
// the merge gate needs the head SHA to anchor the commit status.
func (a *AppClient) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	rest, err := a.rest(ctx)
	if err != nil {
		return forge.PullRef{}, err
	}
	return rest.GetPullRequest(ctx, repo, number)
}

// githubCommitState maps the normalized CommitState onto GitHub's wire
// vocabulary (error|failure|pending|success). An unknown value fails closed to
// "error" so a miswired gate blocks rather than silently passes.
func githubCommitState(s forge.CommitState) string {
	switch s {
	case forge.CommitStateSuccess:
		return "success"
	case forge.CommitStateFailure:
		return "failure"
	case forge.CommitStatePending:
		return "pending"
	default:
		return "error"
	}
}
