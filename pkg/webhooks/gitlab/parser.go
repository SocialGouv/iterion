package gitlab

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Parsed is the normalized merge-request view the handler consumes.
type Parsed struct {
	ProjectID      int64
	ProjectPath    string // group/sub/repo
	ProjectWebURL  string
	CloneURL       string
	MRIID          int64
	Action         string // open|reopen|update|...
	SourceBranch   string
	TargetBranch   string
	Title          string
	Description    string
	MRURL          string
	HeadSHA        string
	OldRev         string
	Labels         []string
	SenderUsername string // the actor that opened/reopened the MR (e.g. "renovate")
}

// ParseMergeRequest decodes a GitLab merge_request webhook body.
func ParseMergeRequest(body []byte) (Parsed, error) {
	var e MergeRequestEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return Parsed{}, fmt.Errorf("gitlab: decode mr event: %w", err)
	}
	if e.ObjectKind != "merge_request" {
		return Parsed{}, fmt.Errorf("gitlab: not a merge_request event (object_kind=%q)", e.ObjectKind)
	}
	oa := e.ObjectAttributes
	labels := make([]string, 0, len(e.Labels))
	for _, l := range e.Labels {
		if l.Title != "" {
			labels = append(labels, l.Title)
		}
	}
	return Parsed{
		ProjectID:      e.Project.ID,
		ProjectPath:    e.Project.PathWithNamespace,
		ProjectWebURL:  e.Project.WebURL,
		CloneURL:       e.Project.GitHTTPURL,
		MRIID:          oa.IID,
		Action:         oa.Action,
		SourceBranch:   oa.SourceBranch,
		TargetBranch:   oa.TargetBranch,
		Title:          oa.Title,
		Description:    oa.Description,
		MRURL:          oa.URL,
		HeadSHA:        oa.LastCommit.ID,
		OldRev:         oa.OldRev,
		Labels:         labels,
		SenderUsername: e.User.Username,
	}, nil
}

// IsReviewable reports whether the MR action should AUTO-trigger a review.
// Only open/reopen do: a review fires once when the MR is created (or
// reopened). Pushes to the MR ("update" with a new head) deliberately do
// NOT re-trigger — auto-review-on-every-push was found too heavy. Re-review
// after a push is on-demand via the `/revi` note command instead.
func (p Parsed) IsReviewable() bool {
	switch p.Action {
	case "open", "reopen":
		return true
	default:
		return false
	}
}

// SubjectID is the stable per-MR identifier used in delivery records.
func (p Parsed) SubjectID() string {
	return "mr:" + strconv.FormatInt(p.MRIID, 10)
}
