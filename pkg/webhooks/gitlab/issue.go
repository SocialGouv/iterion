package gitlab

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// EventHeaderIssue is the value GitLab sends in X-Gitlab-Event for an issue
// lifecycle event ("Issue Hook"). Unlike GitHub there is no dedicated
// "labeled" action — labeling an issue arrives as an action:"update" with the
// added label in changes.labels, so we diff previous→current to detect it.
const EventHeaderIssue = "Issue Hook"

// IssueEvent is the subset of GitLab's issue webhook we decode: the issue
// itself plus the labels diff that tells us which label was just added.
type IssueEvent struct {
	ObjectKind       string          `json:"object_kind"`
	EventType        string          `json:"event_type"`
	User             User            `json:"user"`
	Project          Project         `json:"project"`
	ObjectAttributes IssueAttributes `json:"object_attributes"`
	Labels           []Label         `json:"labels"` // current labels after the change
	Changes          IssueChanges    `json:"changes"`
}

type IssueAttributes struct {
	IID         int64  `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`  // "opened" | "closed"
	Action      string `json:"action"` // "open" | "update" | "close" | "reopen"
	URL         string `json:"url"`
}

// IssueChanges carries the before/after diff GitLab includes on an update.
// Only `labels` is modelled — it is what distinguishes "a label was added"
// from any other issue edit.
type IssueChanges struct {
	Labels *LabelsChange `json:"labels"`
}

type LabelsChange struct {
	Previous []Label `json:"previous"`
	Current  []Label `json:"current"`
}

// ParsedIssue is the normalized issue-lifecycle view the handler consumes:
// the repo to clone, the issue to implement + back-link, and the labels that
// were freshly added on this event.
type ParsedIssue struct {
	ProjectID     int64
	ProjectPath   string
	CloneURL      string
	DefaultBranch string // base ref a command's MR is opened against
	IssueIID      int64
	Action        string // "open" | "update" | "close" | "reopen"
	Title         string
	Description   string
	URL           string // the issue's own web URL — the back-link target
	State         string // "opened" | "closed"
	AddedLabels   []string
	// Labels is the issue's full current label set (titles) after the change —
	// what the bot-agnostic hold-label gate reads (distinct from AddedLabels,
	// which is only the freshly-added trigger).
	Labels         []string
	AuthorID       int64
	AuthorUsername string
}

// ParseIssue decodes a GitLab issue webhook body and computes the
// freshly-added labels (current − previous from changes.labels).
func ParseIssue(body []byte) (ParsedIssue, error) {
	var e IssueEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return ParsedIssue{}, fmt.Errorf("gitlab: decode issue event: %w", err)
	}
	if e.ObjectKind != "issue" {
		return ParsedIssue{}, fmt.Errorf("gitlab: not an issue event (object_kind=%q)", e.ObjectKind)
	}
	oa := e.ObjectAttributes
	return ParsedIssue{
		ProjectID:      e.Project.ID,
		ProjectPath:    e.Project.PathWithNamespace,
		CloneURL:       e.Project.GitHTTPURL,
		DefaultBranch:  e.Project.DefaultBranch,
		IssueIID:       oa.IID,
		Action:         oa.Action,
		Title:          oa.Title,
		Description:    oa.Description,
		URL:            oa.URL,
		State:          oa.State,
		AddedLabels:    addedLabels(e.Changes.Labels),
		Labels:         labelTitles(e.Labels),
		AuthorID:       e.User.ID,
		AuthorUsername: e.User.Username,
	}, nil
}

// labelTitles extracts the non-empty label titles from a GitLab label list.
func labelTitles(labels []Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Title != "" {
			out = append(out, l.Title)
		}
	}
	return out
}

// addedLabels returns the label titles present in current but not previous.
// When the event carries no labels diff (issue open, or a non-label update)
// it returns nil — so only an actual label addition can trigger a launch,
// matching GitHub's `issues.labeled` semantics.
func addedLabels(ch *LabelsChange) []string {
	if ch == nil {
		return nil
	}
	prev := make(map[string]bool, len(ch.Previous))
	for _, l := range ch.Previous {
		prev[l.Title] = true
	}
	var added []string
	for _, l := range ch.Current {
		if l.Title != "" && !prev[l.Title] {
			added = append(added, l.Title)
		}
	}
	return added
}

// SubjectID is the stable per-issue id used in delivery records + idempotency.
func (p ParsedIssue) SubjectID() string {
	return "issue:" + strconv.FormatInt(p.IssueIID, 10)
}
