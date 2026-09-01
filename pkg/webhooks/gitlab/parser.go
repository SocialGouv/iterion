package gitlab

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	State          string // opened|closed|merged|locked
	UpdatedAt      string // object_attributes.updated_at — distinguishes successive update events on one head
	Labels         []string
	SenderUsername string // the actor that opened/reopened the MR (e.g. "renovate")
	// Draft reports whether the MR is currently a work-in-progress draft. A
	// draft MR never auto-triggers a bot (IsReviewable is false).
	Draft bool
	// BecameReady is true when THIS event is the draft→ready transition
	// (GitLab's `changes.draft` went true→false on an `update`). It is the
	// GitLab equivalent of GitHub's `ready_for_review` action — the moment a
	// draft becomes reviewable — since GitLab has no dedicated action for it.
	BecameReady bool
	// ReRequestedReviewers are the usernames whose review was explicitly
	// re-requested by THIS event (the "Re-request review" sidebar button —
	// `changes.reviewers.current[].re_requested`, gitlab-org/gitlab!205274).
	ReRequestedReviewers []string
	// AddedReviewers are the usernames newly present in the reviewer set
	// (current − previous). Adding a reviewer is the same request-a-review
	// gesture on GitLab versions that predate the re_requested attribute —
	// and the fallback trigger when it is absent.
	AddedReviewers []string
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
	// A draft→ready transition is an `update` whose changes show draft (or the
	// deprecated work_in_progress alias) going true→false.
	becameReady := false
	if c := e.Changes.Draft; c != nil && c.Previous && !c.Current {
		becameReady = true
	}
	if c := e.Changes.WorkInProgress; c != nil && c.Previous && !c.Current {
		becameReady = true
	}
	var reRequested, added []string
	if rc := e.Changes.Reviewers; rc != nil {
		prev := make(map[int64]bool, len(rc.Previous))
		for _, r := range rc.Previous {
			prev[r.ID] = true
		}
		for _, r := range rc.Current {
			if r.Username == "" {
				continue
			}
			if r.ReRequested {
				reRequested = append(reRequested, r.Username)
			}
			if !prev[r.ID] {
				added = append(added, r.Username)
			}
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
		State:          oa.State,
		UpdatedAt:      oa.UpdatedAt,
		Labels:         labels,
		SenderUsername: e.User.Username,
		Draft:          oa.Draft || oa.WorkInProgress,
		BecameReady:    becameReady,

		ReRequestedReviewers: reRequested,
		AddedReviewers:       added,
	}, nil
}

// ReviewRequestedFrom reports whether THIS event asks `login` for a review:
// either an explicit "Re-request review" click targeting them, or their
// first addition to the reviewer set (the same gesture, and the only form
// GitLab versions without the re_requested attribute can express).
func (p Parsed) ReviewRequestedFrom(login string) bool {
	for _, u := range p.ReRequestedReviewers {
		if strings.EqualFold(u, login) {
			return true
		}
	}
	for _, u := range p.AddedReviewers {
		if strings.EqualFold(u, login) {
			return true
		}
	}
	return false
}

// IsReviewable reports whether the MR action should AUTO-trigger a review.
// A DRAFT MR is never auto-reviewable — the author is still iterating, so
// auto-running a bot wastes budget and churns an unfinished branch. Otherwise
// a review fires when the MR is created (open), reopened, or marked ready
// (the `update` that clears draft — GitLab's stand-in for a ready action).
// Plain pushes to the MR ("update" with a new head, draft unchanged)
// deliberately do NOT re-trigger — auto-review-on-every-push was found too
// heavy; re-review after a push is on-demand via the `/revi` note command.
func (p Parsed) IsReviewable() bool {
	if p.Draft {
		return false
	}
	switch p.Action {
	case "open", "reopen":
		return true
	case "update":
		return p.BecameReady
	default:
		return false
	}
}

// IsSynchronize reports whether this MR event is a push to the source branch
// (action "update" carrying an OldRev — the code changed, not just metadata).
// IsReviewable excludes it; the merge gate opts back in via ReviewOnSync so the
// revi/review status re-evaluates on the new head.
func (p Parsed) IsSynchronize() bool {
	return !p.Draft && p.Action == "update" && p.OldRev != ""
}

// SubjectID is the stable per-MR identifier used in delivery records.
func (p Parsed) SubjectID() string {
	return "mr:" + strconv.FormatInt(p.MRIID, 10)
}
