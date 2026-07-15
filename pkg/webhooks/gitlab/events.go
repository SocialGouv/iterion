// Package gitlab decodes GitLab webhook payloads into the narrow,
// normalized shape iterion's inbound handler consumes. It deliberately
// models only the fields the merge-request review flow needs — never the
// whole event — so the handler can persist selected fields + a payload
// hash without retaining the raw body.
package gitlab

// EventHeader is the value GitLab sends in X-Gitlab-Event for an MR.
const EventHeaderMergeRequest = "Merge Request Hook"

// MergeRequestEvent is the subset of GitLab's merge_request webhook we
// decode.
type MergeRequestEvent struct {
	ObjectKind       string           `json:"object_kind"`
	EventType        string           `json:"event_type"`
	User             User             `json:"user"` // the actor that opened/reopened the MR
	Project          Project          `json:"project"`
	ObjectAttributes ObjectAttributes `json:"object_attributes"`
	Labels           []Label          `json:"labels"`
	// Changes records the before/after of attributes touched by this event.
	// GitLab has no dedicated "ready_for_review" action — a draft MR marked
	// ready arrives as an `update` whose Changes.Draft went true→false, so
	// the handler must inspect this to distinguish it from a plain push.
	Changes Changes `json:"changes"`
}

// Changes is the top-level `changes` object GitLab sends on `update`
// events. We decode only the draft-transition fields. Pointers so a
// missing key is distinguishable from a present {false,false}.
type Changes struct {
	Draft          *BoolChange `json:"draft"`
	WorkInProgress *BoolChange `json:"work_in_progress"` // deprecated alias GitLab still emits
}

// BoolChange is the {previous,current} pair GitLab uses for a boolean
// attribute inside `changes`.
type BoolChange struct {
	Previous bool `json:"previous"`
	Current  bool `json:"current"`
}

type Project struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	GitHTTPURL        string `json:"git_http_url"`
	DefaultBranch     string `json:"default_branch"`
}

type ObjectAttributes struct {
	IID          int64  `json:"iid"`
	Action       string `json:"action"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	URL          string `json:"url"`
	OldRev       string `json:"oldrev"`
	LastCommit   Commit `json:"last_commit"`
	// Draft is GitLab's work-in-progress flag (14.0+); WorkInProgress is the
	// deprecated alias older GitLab still sends. Either set ⇒ the MR must not
	// auto-launch a bot; the trigger is the update that clears it.
	Draft          bool `json:"draft"`
	WorkInProgress bool `json:"work_in_progress"`
}

type Commit struct {
	ID string `json:"id"`
}

type Label struct {
	Title string `json:"title"`
}
