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
	Labels []Label `json:"labels"`
	Head   Ref     `json:"head"`
	Base   Ref     `json:"base"`
}

type Ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
	// Repo is the repository the branch lives in. For a fork PR, head.repo
	// differs from base.repo (full_name) — the signal the fork guard reads to
	// keep an untrusted fork PR off the mutating auto-launch path.
	Repo Repository `json:"repo"`
}

type Sender struct {
	Login string `json:"login"`
}
