package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// ---------------------------------------------------------------------------
// Merge-gate gestures, GitHub side — the twins of pkg/forge/gitlab/approvals.
// GitHub expresses approval as a review event (the forge has no dedicated
// approve endpoint) and auto-merge as a GraphQL mutation; both are server-side
// gestures of the same publish-surface doctrine as GitLab's.
// ---------------------------------------------------------------------------

// ApprovePullRequest casts the client account's approval
// (forge.MergeApprover) as a review with event APPROVE. The sha parameter is
// accepted for interface parity and deliberately not sent: the REST endpoint
// documents an optional commit_id but does not promise refusing a stale one,
// so the stale-head authority is the publish surface's re-read that runs
// immediately before this call, not the wire. Residual: one round trip.
func (c *AdminClient) ApprovePullRequest(ctx context.Context, repo string, number int, sha string) (forge.ApprovalResult, error) {
	_ = sha // no pin-by-sha approve on GitHub; see doc above
	prPath := "/repos/" + repo + "/pulls/" + strconv.Itoa(number)
	var rev githubReview
	code, errBody, err := c.doErr(ctx, http.MethodPost, prPath+"/reviews",
		map[string]any{"event": "APPROVE"}, &rev)
	if err != nil {
		return forge.ApprovalResult{}, err
	}
	if code/100 != 2 {
		return forge.ApprovalResult{}, refusal("create approving review", code, errBody, "pull_requests:write")
	}
	return forge.ApprovalResult{URL: rev.HTMLURL}, nil
}

// ApprovePullRequest on an App connection rides the pull_requests:write
// profile — an approving review is a pull_requests write, same as the
// comment-only review the profile already mints for.
func (a *AppClient) ApprovePullRequest(ctx context.Context, repo string, number int, sha string) (forge.ApprovalResult, error) {
	c, err := a.scopedREST(ctx, PullWriteInstallationPermissions())
	if err != nil {
		return forge.ApprovalResult{}, err
	}
	return c.ApprovePullRequest(ctx, repo, number, sha)
}

// enableAutoMergeMutation arms GitHub auto-merge. expectedHeadOid is the
// race guard — GitHub refuses the mutation when the PR's head is not the
// audited revision.
const enableAutoMergeMutation = `mutation($id: ID!, $method: PullRequestMergeMethod!, $oid: GitObjectID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method, expectedHeadOid: $oid}) {
    pullRequest { state merged }
  }
}`

// ArmAutoMerge arms GitHub auto-merge (forge.AutoMergeArmer). GitHub's
// auto-merge enforces the repository's own merge requirements before
// completing (required checks, approvals, branch protection), which is the
// same "the forge merges when its own conditions hold" contract as GitLab's
// MWPS.
func (c *AdminClient) ArmAutoMerge(ctx context.Context, repo string, number int, opts forge.AutoMergeOptions) (forge.AutoMergeResult, error) {
	if opts.SHA == "" {
		return forge.AutoMergeResult{}, fmt.Errorf("github: ArmAutoMerge without sha: an unpinned arming is refused by contract")
	}
	prPath := "/repos/" + repo + "/pulls/" + strconv.Itoa(number)
	var pr struct {
		NodeID  string `json:"node_id"`
		HTMLURL string `json:"html_url"`
	}
	code, err := c.do(ctx, http.MethodGet, prPath, nil, &pr)
	if err != nil {
		return forge.AutoMergeResult{}, err
	}
	if code/100 != 2 {
		return forge.AutoMergeResult{}, statusErr("GET pull request (node id)", code)
	}

	method := "MERGE"
	if opts.Method == forge.MergeSquash {
		method = "SQUASH"
	}
	var out struct {
		EnablePullRequestAutoMerge struct {
			PullRequest struct {
				State  string `json:"state"`
				Merged bool   `json:"merged"`
			} `json:"pullRequest"`
		} `json:"enablePullRequestAutoMerge"`
	}
	qerr := c.GraphQL(ctx, enableAutoMergeMutation, map[string]any{
		"id":     pr.NodeID,
		"method": method,
		"oid":    opts.SHA,
	}, &out)
	if qerr != nil {
		return forge.AutoMergeResult{}, mapAutoMergeError("enable auto-merge", opts.SHA, qerr)
	}
	state := forge.AutoMergeArmed
	if out.EnablePullRequestAutoMerge.PullRequest.Merged {
		state = forge.AutoMergeMerged
	}
	return forge.AutoMergeResult{State: state, URL: pr.HTMLURL, SHA: opts.SHA}, nil
}

// mapAutoMergeError maps a GraphQL refusal onto the forge sentinels the
// publish surface reports: eligibility, staleness, not-mergeable — in that
// order, because the same message can carry more than one clue.
func mapAutoMergeError(op, sha string, err error) error {
	var gerrs *GraphQLErrors
	if !errors.As(err, &gerrs) {
		return err
	}
	msg := strings.ToLower(gerrs.Error())
	switch {
	case gerrs.HasType("FORBIDDEN") || strings.Contains(msg, "not authorized") || strings.Contains(msg, "permission"):
		return &forge.PermissionError{
			Provider: forge.ProviderGitHub, Op: op,
			Missing: []string{"pull_requests:write"},
			Remedy:  "approve the pull_requests write grant on the installation — auto-merge arming is a pull request write",
			Cause:   err,
		}
	case strings.Contains(msg, "expected head oid") || strings.Contains(msg, "head oid") || strings.Contains(msg, "head sha was not equal"):
		return fmt.Errorf("%w: audited %q is no longer the head (%s)", forge.ErrStaleHead, sha, gerrs.Error())
	case strings.Contains(msg, "not mergeable") || strings.Contains(msg, "cannot be merged") || strings.Contains(msg, "mergeable state") || strings.Contains(msg, "clean status"):
		return fmt.Errorf("%w: %s", forge.ErrNotMergeable, gerrs.Error())
	default:
		return err
	}
}

// ArmAutoMerge on an App connection mints a pull_requests:write token and
// rides the AdminClient path — the GraphQL mutation and the node-id fetch
// are both pull-request writes/reads on the same profile.
func (a *AppClient) ArmAutoMerge(ctx context.Context, repo string, number int, opts forge.AutoMergeOptions) (forge.AutoMergeResult, error) {
	c, err := a.scopedREST(ctx, PullWriteInstallationPermissions())
	if err != nil {
		return forge.AutoMergeResult{}, err
	}
	return c.ArmAutoMerge(ctx, repo, number, opts)
}

// AddPullReviewers puts the named accounts on the PR's requested-reviewers
// set (forge.PullReviewerSetter). Read-then-diff, mirroring
// WithdrawPullReviewRequests: the pending set is fetched first and only the
// missing logins are requested, which makes a repeat call a no-op instead of
// a 422, and can never drop a reviewer it was not named.
func (c *AdminClient) AddPullReviewers(ctx context.Context, repo string, number int, logins []string) ([]string, error) {
	want := normalizeLogins(logins)
	if len(want) == 0 {
		return nil, nil
	}
	prPath := "/repos/" + repo + "/pulls/" + strconv.Itoa(number) + "/requested_reviewers"
	var pending requestedReviewersWire
	code, body, err := c.doErr(ctx, http.MethodGet, prPath, nil, &pending)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, refusal("GET requested reviewers", code, body, "pull_requests:read")
	}
	present := make(map[string]struct{}, len(pending.Users))
	for _, u := range pending.Users {
		present[strings.ToLower(u.Login)] = struct{}{}
	}
	missing := make([]string, 0, len(want))
	for key, login := range want {
		if _, ok := present[key]; !ok {
			missing = append(missing, login)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	code, body, err = c.doErr(ctx, http.MethodPost, prPath, map[string]any{"reviewers": missing}, nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, refusal("POST requested reviewers", code, body, "pull_requests:write")
	}
	return missing, nil
}

// AddPullReviewers on an App connection rides the pull_requests:write
// profile (write implies the read the pending-set fetch needs), same shape
// as WithdrawPullReviewRequests.
func (a *AppClient) AddPullReviewers(ctx context.Context, repo string, number int, logins []string) ([]string, error) {
	c, err := a.scopedREST(ctx, PullWriteInstallationPermissions())
	if err != nil {
		return nil, err
	}
	return c.AddPullReviewers(ctx, repo, number, logins)
}

// GetMergeability reads GitHub's own merge verdict
// (forge.MergeabilityReader). `mergeable` is tri-state on the wire
// (true/false/null — null while GitHub computes); mergeable_state refines
// it. Approvals counts stay -1: they would need a reviews read per call and
// the field is diagnostic, never load-bearing.
func (c *AdminClient) GetMergeability(ctx context.Context, repo string, number int) (forge.Mergeability, error) {
	var pr struct {
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
	}
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, &pr)
	if err != nil {
		return forge.Mergeability{}, err
	}
	if code/100 != 2 {
		return forge.Mergeability{}, statusErr("GET pull request", code)
	}
	out := forge.Mergeability{
		MergeStatus:           forge.MergeabilityUnknown,
		UnresolvedDiscussions: -1,
		ApprovalsRequired:     -1,
		ApprovalsLeft:         -1,
		Detailed:              pr.MergeableState,
	}
	switch {
	case pr.Mergeable == nil:
		out.MergeStatus = forge.MergeabilityPending
	case *pr.Mergeable:
		out.MergeStatus = forge.MergeabilityMergeable
	default:
		out.MergeStatus = forge.MergeabilityBlocked
	}
	return out, nil
}

// GetMergeability on an App connection reads under the pull_requests:read
// profile.
func (a *AppClient) GetMergeability(ctx context.Context, repo string, number int) (forge.Mergeability, error) {
	c, err := a.scopedREST(ctx, map[string]string{"pull_requests": "read", "metadata": "read"})
	if err != nil {
		return forge.Mergeability{}, err
	}
	return c.GetMergeability(ctx, repo, number)
}

// normalizeLogins lowercases the keys for set math and keeps the LAST input
// casing per login for the payload — GitHub matches case-insensitively and
// echoing a stable casing keeps logs readable.
func normalizeLogins(in []string) map[string]string {
	out := make(map[string]string, len(in))
	for _, l := range in {
		if l = strings.TrimSpace(strings.TrimPrefix(l, "@")); l != "" {
			out[strings.ToLower(l)] = l
		}
	}
	return out
}

var (
	_ forge.MergeApprover      = (*AdminClient)(nil)
	_ forge.AutoMergeArmer     = (*AdminClient)(nil)
	_ forge.PullReviewerSetter = (*AdminClient)(nil)
	_ forge.MergeabilityReader = (*AdminClient)(nil)
	_ forge.MergeApprover      = (*AppClient)(nil)
	_ forge.AutoMergeArmer     = (*AppClient)(nil)
	_ forge.PullReviewerSetter = (*AppClient)(nil)
	_ forge.MergeabilityReader = (*AppClient)(nil)
)
