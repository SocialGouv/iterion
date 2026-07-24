package forgejo

import (
	"context"
	"net/http"
	"net/url"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// SetCommitStatus posts a commit status onto sha via the Forgejo/Gitea
// commit-status API (POST /repos/{repo}/statuses/{sha}). The `context` names
// the check that branch protection's "status check" list matches on. Needs
// `write:repository` on the connection token.
func (c *AdminClient) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	body := map[string]string{
		"state":       forgejoCommitState(st.State),
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
		"/repos/"+repo+"/statuses/"+url.PathEscape(sha), body, &out)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return statusErr("set commit status", code)
	}
	return nil
}

// forgejoCommitState maps the normalized CommitState onto Forgejo/Gitea's wire
// vocabulary (pending|success|error|failure|warning). Unknown → "error" so a
// miswired gate blocks rather than silently passes.
func forgejoCommitState(s forge.CommitState) string {
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
