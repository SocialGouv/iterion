package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// DispatchWorkflow dispatches the workflow file `workflow` on ref
// (forge.WorkflowDispatcher): POST
// /repos/{owner}/{repo}/actions/workflows/{workflow}/dispatches with no
// inputs. An empty ref is resolved to the repository's default branch first
// (GET /repos/{owner}/{repo}), so a renamed default branch is followed, never
// guessed.
//
// GitHub answers 204 on success. A 404 is an unknown workflow (or one the
// credential may not see), a 422 a workflow that declares no
// workflow_dispatch trigger or a ref that does not exist; every answered
// refusal wraps forge.ErrWorkflowNotDispatched. A transport failure on the
// POST does not: the forge may have queued the run.
func (c *AdminClient) DispatchWorkflow(ctx context.Context, repo, workflow, ref string) (forge.WorkflowDispatch, error) {
	if !forge.ValidWorkflowFile(workflow) {
		return forge.WorkflowDispatch{}, fmt.Errorf("%w: github: workflow %q is not a plain *.yml/*.yaml file name", forge.ErrWorkflowNotDispatched, workflow)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		var info struct {
			DefaultBranch string `json:"default_branch"`
		}
		code, errBody, err := c.doErr(ctx, http.MethodGet, "/repos/"+repo, nil, &info)
		if err != nil {
			return forge.WorkflowDispatch{}, fmt.Errorf("%w: github: read the default branch of %s: %w", forge.ErrWorkflowNotDispatched, repo, err)
		}
		if code/100 != 2 {
			return forge.WorkflowDispatch{}, fmt.Errorf("%w: %w", forge.ErrWorkflowNotDispatched,
				refusal("get repository (default branch)", code, errBody, "metadata:read"))
		}
		ref = strings.TrimSpace(info.DefaultBranch)
		if ref == "" {
			return forge.WorkflowDispatch{}, fmt.Errorf("%w: github: %s reports no default branch", forge.ErrWorkflowNotDispatched, repo)
		}
	}
	p := "/repos/" + repo + "/actions/workflows/" + url.PathEscape(workflow) + "/dispatches"
	code, errBody, err := c.doErr(ctx, http.MethodPost, p, map[string]any{"ref": ref}, nil)
	if err != nil {
		// No answer, or a redirected write: the forge may have queued it.
		return forge.WorkflowDispatch{}, fmt.Errorf("github: dispatch workflow %s on %s@%s: %w", workflow, repo, ref, err)
	}
	if code/100 != 2 {
		cause := refusal("dispatch workflow", code, errBody, PermissionActions+":write")
		if msg := githubErrorMessage(errBody); msg != "" && code != http.StatusForbidden {
			cause = fmt.Errorf("%w: %s", cause, msg)
		}
		return forge.WorkflowDispatch{}, fmt.Errorf("%w: %w", forge.ErrWorkflowNotDispatched, cause)
	}
	return forge.WorkflowDispatch{Ref: ref}, nil
}

var _ forge.WorkflowDispatcher = (*AdminClient)(nil)
