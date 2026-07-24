package gitlab

import (
	"context"
	"net/http"
	"net/url"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// SetCommitStatus posts a commit status onto sha via the GitLab commit-status
// API (POST /projects/:id/statuses/:sha). GitLab names the check via `name`
// (the equivalent of GitHub's `context`); a merge-request approval/pipeline
// rule referencing that name blocks the MR until it is green. Needs `api`
// scope on the connection token.
func (c *AdminClient) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	body := map[string]string{
		"state":       gitlabCommitState(st.State),
		"name":        st.Context,
		"context":     st.Context,
		"description": forge.TruncateStatusDescription(st.Description),
	}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	var out struct {
		ID int64 `json:"id"`
	}
	code, err := c.do(ctx, http.MethodPost,
		"/projects/"+projectID(repo)+"/statuses/"+url.PathEscape(sha), body, &out)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("set commit status", code)
	}
	return nil
}

// gitlabCommitState maps the normalized CommitState onto GitLab's wire
// vocabulary (pending|running|success|failed|canceled). Unknown → "failed"
// so a miswired gate blocks rather than silently passes.
func gitlabCommitState(s forge.CommitState) string {
	switch s {
	case forge.CommitStateSuccess:
		return "success"
	case forge.CommitStatePending:
		return "pending"
	default:
		return "failed"
	}
}
