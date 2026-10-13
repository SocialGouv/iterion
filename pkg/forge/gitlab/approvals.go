package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// approveEligibility is the remedy text for an approval the forge refused on
// eligibility: the credential is valid, the bot user is just not allowed to
// approve THIS merge request.
const approveEligibility = "the bot user must be eligible to approve this merge request — check the project's approval rules " +
	"(Settings → Merge requests → Approval rules) and that the token's role meets them; a project that excludes bot users " +
	"from approvals needs a rule change before a gating bot can approve"

// approve is the shared approve call: POST /merge_requests/{iid}/approve with
// an optional sha pin. 201/200 = approved; a refusal whose body says the MR
// is already approved is the idempotent no-op the contract promises; a 403 is
// an eligibility question (PermissionError, not a bare 403).
func (c *AdminClient) approve(ctx context.Context, repo string, number int, sha string) (forge.ApprovalResult, error) {
	mrPath := "/projects/" + projectID(repo) + "/merge_requests/" + strconv.Itoa(number)
	body := map[string]any{}
	if sha != "" {
		body["sha"] = sha
	}
	var out struct {
		WebURL string `json:"web_url"`
	}
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodPost, mrPath+"/approve", body, &out)
	if err != nil {
		return forge.ApprovalResult{}, err
	}
	if code == http.StatusOK || code == http.StatusCreated {
		return forge.ApprovalResult{URL: out.WebURL}, nil
	}
	msg := strings.ToLower(string(errBody))
	switch {
	case strings.Contains(msg, "already approved"):
		// Idempotent re-approve: the approval exists, which is what the
		// caller asked to be true.
		return forge.ApprovalResult{URL: out.WebURL}, nil
	case code == http.StatusForbidden:
		return forge.ApprovalResult{}, &forge.PermissionError{
			Provider: forge.ProviderGitLab, Op: "POST merge request approve",
			Missing: []string{"api: approvals"},
			Remedy:  approveEligibility,
			Cause:   fmt.Errorf("%w: %s", forge.ErrForbidden, strings.TrimSpace(string(errBody))),
		}
	case code == http.StatusNotAcceptable || code == http.StatusConflict:
		return forge.ApprovalResult{}, fmt.Errorf("%w: approve pinned to %q refused by the forge", forge.ErrStaleHead, sha)
	default:
		return forge.ApprovalResult{}, statusErr("POST merge request approve", code)
	}
}

// ApprovePullRequest casts the token account's approval (forge.MergeApprover).
// sha pins the approval to the audited head where the instance supports it;
// an instance that ignores the field still anchors approvals to the current
// head by itself.
func (c *AdminClient) ApprovePullRequest(ctx context.Context, repo string, number int, sha string) (forge.ApprovalResult, error) {
	return c.approve(ctx, repo, number, sha)
}

// ArmAutoMerge arms merge-when-pipeline-succeeds (forge.AutoMergeArmer) via
// the merge endpoint's MWPS mode: the forge merges itself once its own merge
// conditions hold (pipeline green per the project's settings, no conflicts).
// opts.SHA is the race guard: GitLab answers 406 when the MR's head is no
// longer the audited revision.
func (c *AdminClient) ArmAutoMerge(ctx context.Context, repo string, number int, opts forge.AutoMergeOptions) (forge.AutoMergeResult, error) {
	if opts.SHA == "" {
		return forge.AutoMergeResult{}, fmt.Errorf("gitlab: ArmAutoMerge without sha: an unpinned arming is refused by contract")
	}
	mrPath := "/projects/" + projectID(repo) + "/merge_requests/" + strconv.Itoa(number) + "/merge"
	body := map[string]any{
		"merge_when_pipeline_succeeds": true,
		"sha":                          opts.SHA,
	}
	if opts.Method == forge.MergeSquash {
		body["squash"] = true
	}
	if opts.DeleteBranch {
		body["should_remove_source_branch"] = true
	}
	var mr gitlabMR
	// Some GitLab versions answer 202 Accepted while the arming is recorded;
	// anything in the 2xx family means the merge endpoint took the request.
	code, errBody, err := c.http().DoErrBody(ctx, http.MethodPut, mrPath, body, &mr)
	if err != nil {
		return forge.AutoMergeResult{}, err
	}
	if code/100 != 2 {
		switch code {
		case http.StatusMethodNotAllowed:
			return forge.AutoMergeResult{}, fmt.Errorf("%w: %s", forge.ErrNotMergeable, strings.TrimSpace(string(errBody)))
		case http.StatusNotAcceptable:
			return forge.AutoMergeResult{}, fmt.Errorf("%w: audited %q is no longer the head", forge.ErrStaleHead, opts.SHA)
		case http.StatusForbidden:
			return forge.AutoMergeResult{}, &forge.PermissionError{
				Provider: forge.ProviderGitLab, Op: "PUT merge request merge (MWPS)",
				Missing: []string{"api: merge"},
				Remedy:  "the token's role must be able to merge this merge request (typically Maintainer, or Developer where the project allows)",
				Cause:   fmt.Errorf("%w: %s", forge.ErrForbidden, strings.TrimSpace(string(errBody))),
			}
		default:
			return forge.AutoMergeResult{}, statusErr("PUT merge request merge (MWPS)", code)
		}
	}
	state := forge.AutoMergeArmed
	if mr.State == "merged" {
		state = forge.AutoMergeMerged
	}
	return forge.AutoMergeResult{State: state, URL: mr.WebURL, SHA: mr.SHA}, nil
}

// AddPullReviewers puts the named accounts on the MR's reviewer set
// (forge.PullReviewerSetter). Same read-modify-write contract as
// AddSelfAsPullReviewer — GitLab's reviewer_ids PUT REPLACES the set — with
// one extra read per login to resolve usernames to ids.
func (c *AdminClient) AddPullReviewers(ctx context.Context, repo string, number int, logins []string) ([]string, error) {
	logins = uniqueNonEmpty(logins)
	if len(logins) == 0 {
		return nil, nil
	}
	ids := make(map[int64]string, len(logins))
	for _, login := range logins {
		id, err := c.userIDByUsername(ctx, login)
		if err != nil {
			return nil, fmt.Errorf("resolve reviewer %q: %w", login, err)
		}
		ids[id] = login
	}

	mrPath := "/projects/" + projectID(repo) + "/merge_requests/" + strconv.Itoa(number)
	var mr struct {
		Reviewers *[]struct {
			ID int64 `json:"id"`
		} `json:"reviewers"`
	}
	code, err := c.do(ctx, http.MethodGet, mrPath, nil, &mr)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("get merge request reviewers", code)
	}
	if mr.Reviewers == nil {
		// The PUT below REPLACES the reviewer set; a response with no
		// reviewers field cannot prove the write additive. Same refusal as
		// AddSelfAsPullReviewer.
		return nil, fmt.Errorf("merge request response carries no reviewers field — refusing a replace-write that cannot prove itself additive")
	}
	union := make([]int64, 0, len(*mr.Reviewers)+len(ids))
	added := make([]string, 0, len(ids))
	for _, r := range *mr.Reviewers {
		union = append(union, r.ID)
		delete(ids, r.ID) // already a reviewer — not "added"
	}
	for id, login := range ids {
		union = append(union, id)
		added = append(added, login)
	}
	if len(added) == 0 {
		return nil, nil // everyone already on the set — no write at all
	}
	code, err = c.do(ctx, http.MethodPut, mrPath, map[string]any{"reviewer_ids": union}, &struct{}{})
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("set merge request reviewers", code)
	}
	return added, nil
}

// userIDByUsername resolves one GitLab username to a user id. The /users
// lookup filters by username; an exact-match filter is applied anyway so a
// listing quirk can never put the wrong account on an MR, and a miss is an
// error — fail closed, never a silent skip.
func (c *AdminClient) userIDByUsername(ctx context.Context, username string) (int64, error) {
	var users []struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	code, err := c.do(ctx, http.MethodGet, "/users?username="+url.QueryEscape(strings.TrimPrefix(username, "@")), nil, &users)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, statusErr("GET /users?username", code)
	}
	matches := 0
	id := int64(0)
	for _, u := range users {
		if u.Username == strings.TrimPrefix(username, "@") {
			matches++
			id = u.ID
		}
	}
	if matches != 1 {
		return 0, fmt.Errorf("username %q resolved to %d users on the forge, want exactly 1", username, matches)
	}
	if id <= 0 {
		return 0, fmt.Errorf("username %q resolved to user id %d, not a usable GitLab user id", username, id)
	}
	return id, nil
}

// GetMergeability reads the forge's own merge verdict
// (forge.MergeabilityReader). Every half of the read degrades independently:
// the approvals read is EE/CE-version-dependent and its absence means -1
// ("the forge does not say"), never a failed read.
func (c *AdminClient) GetMergeability(ctx context.Context, repo string, number int) (forge.Mergeability, error) {
	mrPath := "/projects/" + projectID(repo) + "/merge_requests/" + strconv.Itoa(number)
	var mr gitlabMR
	code, err := c.do(ctx, http.MethodGet, mrPath, nil, &mr)
	if err != nil {
		return forge.Mergeability{}, err
	}
	if code != http.StatusOK {
		return forge.Mergeability{}, statusErr("get merge request", code)
	}
	out := forge.Mergeability{
		MergeStatus:           normalizeMergeStatus(mr.MergeStatus, mr.DetailedMergeStatus),
		Detailed:              mr.DetailedMergeStatus,
		UnresolvedDiscussions: 0,
		ApprovalsRequired:     -1,
		ApprovalsLeft:         -1,
	}
	if mr.BlockingDiscussionsResolved != nil && !*mr.BlockingDiscussionsResolved {
		out.UnresolvedDiscussions = 1 // the forge reports a boolean, not a count
	}
	var ap struct {
		ApprovalsRequired int `json:"approvals_required"`
		ApprovalsLeft     int `json:"approvals_left"`
	}
	code, err = c.do(ctx, http.MethodGet, mrPath+"/approvals", nil, &ap)
	if err == nil && code == http.StatusOK {
		out.ApprovalsRequired = ap.ApprovalsRequired
		out.ApprovalsLeft = ap.ApprovalsLeft
	}
	return out, nil
}

// normalizeMergeStatus maps GitLab's two merge-status fields onto the forge
// vocabulary. detailed_merge_status (GitLab ≥ 15.6) is authoritative when
// present; the legacy merge_status is the fallback, and neither → unknown.
func normalizeMergeStatus(legacy, detailed string) string {
	switch detailed {
	case "mergeable":
		return forge.MergeabilityMergeable
	case "ci_still_running", "ci_must_pass", "checking":
		return forge.MergeabilityPending
	case "":
		// fall through to the legacy field
	default:
		// conflicts, discussions_not_resolved, not_open, draft_status, …
		return forge.MergeabilityBlocked
	}
	switch legacy {
	case "can_be_merged":
		return forge.MergeabilityMergeable
	case "cannot_be_merged", "cannot_be_merged_recheck":
		return forge.MergeabilityBlocked
	case "checking":
		return forge.MergeabilityPending
	default:
		return forge.MergeabilityUnknown
	}
}

// uniqueNonEmpty normalizes the inputs the way the forge names accounts (no
// leading @), collapses duplicates so a doubled login costs one lookup, and
// drops empties.
func uniqueNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@"))
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

var (
	_ forge.MergeApprover      = (*AdminClient)(nil)
	_ forge.AutoMergeArmer     = (*AdminClient)(nil)
	_ forge.PullReviewerSetter = (*AdminClient)(nil)
	_ forge.MergeabilityReader = (*AdminClient)(nil)
)
