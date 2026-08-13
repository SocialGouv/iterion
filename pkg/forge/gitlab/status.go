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

// ListCommitStatuses returns the statuses already present on sha
// (GET /projects/:id/repository/commits/:sha/statuses). Reading before
// writing is what lets a reconciler tell "no verdict was ever posted" from
// "a verdict is already there" — without it, repairing a missing check could
// overwrite a real one.
//
// GitLab's default scope already serves the latest row per name (history
// needs all=true, which this deliberately does not pass) — verified live on
// GitLab 19.2. The per-name dedup keeping the newest row (highest id) is
// defense in depth for the shapes the default scope can still produce (one
// row per pipeline carrying the same name): a gate reader handed more than
// one row per name could match a stale verdict first.
func (c *AdminClient) ListCommitStatuses(ctx context.Context, repo, sha string) ([]forge.CommitStatus, error) {
	var rows []struct {
		ID          int64  `json:"id"`
		Status      string `json:"status"`
		Name        string `json:"name"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}
	code, err := c.do(ctx, http.MethodGet,
		"/projects/"+projectID(repo)+"/repository/commits/"+url.PathEscape(sha)+"/statuses?per_page=100", nil, &rows)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, statusErr("list commit statuses", code)
	}
	byName := map[string]int{} // name → index into out/ids
	out := make([]forge.CommitStatus, 0, len(rows))
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		st := forge.CommitStatus{
			State:       commitStateFromGitLab(r.Status),
			Context:     r.Name,
			Description: r.Description,
			TargetURL:   r.TargetURL,
		}
		if i, ok := byName[r.Name]; ok {
			if r.ID > ids[i] {
				out[i], ids[i] = st, r.ID
			}
			continue
		}
		byName[r.Name] = len(out)
		out = append(out, st)
		ids = append(ids, r.ID)
	}
	return out, nil
}

// commitStateFromGitLab maps GitLab's wire status vocabulary back onto the
// normalized CommitState — the read-side inverse of gitlabCommitState,
// widened to the job states GitLab reports but iterion never posts.
// In-flight states read as pending; canceled/skipped/unknown read as error:
// an abandoned check is unevaluated, never a verdict.
func commitStateFromGitLab(s string) forge.CommitState {
	switch s {
	case "success":
		return forge.CommitStateSuccess
	case "failed":
		return forge.CommitStateFailure
	case "pending", "created", "running", "waiting_for_resource", "preparing", "scheduled", "manual":
		return forge.CommitStatePending
	default: // canceled, skipped, anything future
		return forge.CommitStateError
	}
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
	case forge.CommitStateFailure, forge.CommitStateError:
		return "failed"
	default:
		return "failed" // unknown fails closed
	}
}
