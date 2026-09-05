// Package prforge decodes pull_request webhook payloads from PR-over-forge
// providers — GitHub and Forgejo/Gitea — which share the same wire shape
// for the pull_request event. We model only the fields iterion's inbound
// handler consumes for the review-PR flow (never the whole event) so the
// handler can persist selected fields + a payload hash without retaining
// the raw body.
//
// GitLab is intentionally NOT covered here: its merge_request wire shape
// differs and lives in pkg/webhooks/gitlab.
package prforge

import "encoding/json"

// EventHeaderPullRequest is the X-{GitHub,Forgejo,Gitea}-Event value for
// a PR event. Both forge families also send events like "ping", "push",
// "issue_comment" on the same URL; the handler filters on this constant.
const EventHeaderPullRequest = "pull_request"

// PullRequestEvent is the subset of the pull_request webhook payload we
// decode. Field names follow the wire's camelCase pattern; the shape is
// identical between GitHub and Forgejo/Gitea for the fields we read.
type PullRequestEvent struct {
	Action string `json:"action"`
	Number int64  `json:"number"`
	// Reason is set on a `dequeued` action — why the PR left the merge
	// queue (e.g. "MERGE_CONFLICT", "CI_FAILURE", "INVALID_MERGE_COMMIT").
	// Empty for every other action.
	Reason      string      `json:"reason"`
	Repository  Repository  `json:"repository"`
	PullRequest PullRequest `json:"pull_request"`
	Sender      Sender      `json:"sender"`
	// RequestedReviewer is set on a `review_requested` action (GitHub /
	// Forgejo "Request review" / "Re-request review"): the user whose
	// review this event asks for. Empty on every other action, and when a
	// review is requested from a TEAM (requested_team) instead of a user.
	RequestedReviewer Sender `json:"requested_reviewer"`
}

type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"` // "owner/repo"
	CloneURL string `json:"clone_url"`
	HTMLURL  string `json:"html_url"`
}

type PullRequest struct {
	Number  int64  `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	// Draft is GitHub's / Gitea's work-in-progress flag. A draft PR must not
	// auto-launch a bot — the author is still iterating; the trigger is the
	// `ready_for_review` action (or open/reopen while not draft).
	Draft bool `json:"draft"`
	// User is who OPENED the pull request. Distinct from the event sender on
	// every action but `opened`: on a push to an existing PR the sender is
	// whoever pushed, which is not who the PR belongs to.
	User Sender `json:"user"`
	// Labels is the PR's current label set. GitHub/Gitea include it on the
	// pull_request object; GitLab and minimal payloads omit it (empty → the
	// hold-label suppression fail-opens). Read by the generic hold-label gate.
	Labels    []Label `json:"labels"`
	Head      Ref     `json:"head"`
	Base      Ref     `json:"base"`
	UpdatedAt string  `json:"updated_at"`
}

type Ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
	// Repo is the repository the branch lives in. For a fork PR, head.repo
	// differs from base.repo (full_name) — the signal the fork guard reads to
	// keep an untrusted fork PR off the mutating auto-launch path.
	Repo Repository `json:"repo"`
	// RepoDeclared reports whether the payload CARRIED a `repo` key here —
	// true even when its value is `null`. The two absences are not the same
	// fact and a plain struct (or a pointer) collapses them: a forge that
	// models forks sends `"repo": null` when the head repo was DELETED or
	// blocked, which is a fork whose identity is gone, while a payload that
	// omits the key entirely is a legacy/minimal sender that never had one.
	// The fork guard refuses the first and admits the second.
	RepoDeclared bool `json:"-"`
}

// UnmarshalJSON decodes a Ref and records whether `repo` was present, which
// the generated decoding cannot express (both `null` and absent yield the
// zero value). See RepoDeclared.
func (r *Ref) UnmarshalJSON(b []byte) error {
	type plain Ref // no method set ⇒ no recursion
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	_, declared := keys["repo"]
	*r = Ref(v)
	r.RepoDeclared = declared
	return nil
}

type Sender struct {
	Login string `json:"login"`
}
