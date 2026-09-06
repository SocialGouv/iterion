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
	case forge.CommitStateError:
		return "error"
	default:
		return "error" // unknown fails closed
	}
}

// ListCommitStatuses returns the statuses already present on sha
// (GET /repos/{repo}/commits/{sha}/status). Reading before writing is what
// lets a reconciler tell "no verdict was ever posted" from "a verdict is
// already there" — without it, repairing a missing check could overwrite a
// real one.
func (c *AdminClient) ListCommitStatuses(ctx context.Context, repo, sha string) ([]forge.CommitStatus, error) {
	var out struct {
		Statuses []struct {
			State       string `json:"state"`
			Context     string `json:"context"`
			Description string `json:"description"`
			TargetURL   string `json:"target_url"`
		} `json:"statuses"`
	}
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+url.PathEscape(sha)+"/status", nil, &out)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, statusErr("list commit statuses", code)
	}
	sts := make([]forge.CommitStatus, 0, len(out.Statuses))
	for _, s := range out.Statuses {
		sts = append(sts, forge.CommitStatus{
			State:       forge.CommitState(s.State),
			Context:     s.Context,
			Description: s.Description,
			TargetURL:   s.TargetURL,
		})
	}
	return sts, nil
}

// ListCommitStatuses on an App connection delegates to a management-token
// AdminClient, like SetCommitStatus.
func (a *AppClient) ListCommitStatuses(ctx context.Context, repo, sha string) ([]forge.CommitStatus, error) {
	rest, err := a.rest(ctx)
	if err != nil {
		return nil, err
	}
	return rest.ListCommitStatuses(ctx, repo, sha)
}
