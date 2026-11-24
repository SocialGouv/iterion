package gitlab

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// EventHeaderNote is the value GitLab sends in X-Gitlab-Event for a
// comment / discussion reply ("Note Hook"). It is how a user "talks back"
// to the bot — a reply in the bot's discussion thread or a `/revi`
// command on the MR.
const EventHeaderNote = "Note Hook"

// NoteEvent is the subset of GitLab's note webhook we decode. We model MR
// notes (the conversational /revi flow) and Issue notes (the /command →
// open-MR-and-back-link flow); commit/snippet notes are filtered out upstream.
type NoteEvent struct {
	ObjectKind       string                `json:"object_kind"`
	EventType        string                `json:"event_type"`
	User             User                  `json:"user"`
	Project          Project               `json:"project"`
	ObjectAttributes NoteAttributes        `json:"object_attributes"`
	MergeRequest     *MergeRequestNoteable `json:"merge_request"`
	Issue            *IssueNoteable        `json:"issue"`
}

// User is the GitLab account that authored the note — the candidate
// "replier" the handler authorizes (role-gate + allowlist), and whose
// identity the loop-prevention check compares against the bot.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

type NoteAttributes struct {
	ID           int64         `json:"id"`
	Note         string        `json:"note"`
	NoteableType string        `json:"noteable_type"` // "MergeRequest" | "Issue" | "Commit" | ...
	DiscussionID string        `json:"discussion_id"`
	AuthorID     int64         `json:"author_id"`
	URL          string        `json:"url"`
	Position     *NotePosition `json:"position"`
}

// NotePosition is the diff position GitLab attaches to the note that OPENS a
// thread on the MR's diff (a review comment on a line): which file, which
// lines on which image, and the commit (head_sha) the position was taken
// on. Replies in the thread carry no position — only the root does — which
// is what makes "the note that carries a position" the thread's root
// (ThreadRootAnchor). old_line/new_line: the pre-image and post-image line
// numbers; a comment on a removed line carries only old_line, on an added
// or unchanged one new_line (with old_line beside it when the line existed
// before). old_path/new_path differ when the change renames the file.
type NotePosition struct {
	BaseSHA      string `json:"base_sha"`
	StartSHA     string `json:"start_sha"`
	HeadSHA      string `json:"head_sha"`
	OldPath      string `json:"old_path"`
	NewPath      string `json:"new_path"`
	OldLine      *int64 `json:"old_line"`
	NewLine      *int64 `json:"new_line"`
	PositionType string `json:"position_type"` // "text" | "file"
}

// MergeRequestNoteable is the MR a note is attached to (present on MR notes).
type MergeRequestNoteable struct {
	IID          int64  `json:"iid"`
	State        string `json:"state"` // "opened" | "closed" | "merged" — gates re-review
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	URL          string `json:"url"`
	LastCommit   Commit `json:"last_commit"`
}

// IssueNoteable is the issue a note is attached to (present on Issue notes —
// the surface a human uses to fire a /command that opens an MR back-linking
// the issue).
type IssueNoteable struct {
	IID         int64  `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	State       string `json:"state"` // "opened" | "closed" — gates command handling
}

// ParsedNote is the normalized note view the handler consumes — the MR it
// targets, the discussion thread to reply in, the author to authorize, and
// the note body.
type ParsedNote struct {
	ProjectID    int64
	ProjectPath  string
	CloneURL     string
	MRIID        int64
	SourceBranch string
	TargetBranch string
	MRTitle      string
	MRDesc       string
	MRURL        string
	HeadSHA      string

	// MRState gates command handling: re-review only acts on "opened"
	// MRs (closed/merged notes are filtered, not errors).
	MRState string

	// Issue fields — set when the note is attached to an Issue (the
	// /command-opens-MR surface) instead of an MR. IssueURL is the subject
	// back-linked as source_issue_ref; DefaultBranch (from the project) is the
	// base ref a command's MR is opened against (an issue note carries no
	// source/target branch of its own).
	IssueIID      int64
	IssueTitle    string
	IssueDesc     string
	IssueURL      string
	IssueState    string // "opened" | "closed" — gates command handling
	DefaultBranch string

	NoteID       int64
	NoteBody     string
	DiscussionID string // the thread to reply in
	NoteURL      string

	// Position is the note's own diff position, set only on the note that
	// opened a diff thread (a review comment on a line); a reply inside the
	// thread carries none. ThreadAnchor() normalizes it to the launch shape.
	Position *NotePosition

	AuthorID       int64
	AuthorUsername string
}

// ParseNote decodes a GitLab note webhook body.
func ParseNote(body []byte) (ParsedNote, error) {
	var e NoteEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return ParsedNote{}, fmt.Errorf("gitlab: decode note event: %w", err)
	}
	if e.ObjectKind != "note" {
		return ParsedNote{}, fmt.Errorf("gitlab: not a note event (object_kind=%q)", e.ObjectKind)
	}
	oa := e.ObjectAttributes
	p := ParsedNote{
		ProjectID:      e.Project.ID,
		ProjectPath:    e.Project.PathWithNamespace,
		CloneURL:       e.Project.GitHTTPURL,
		DefaultBranch:  e.Project.DefaultBranch,
		NoteID:         oa.ID,
		NoteBody:       oa.Note,
		DiscussionID:   oa.DiscussionID,
		NoteURL:        oa.URL,
		Position:       oa.Position,
		AuthorID:       e.User.ID,
		AuthorUsername: e.User.Username,
	}
	// GitLab note-hook payloads don't always carry project.git_http_url
	// (issue notes in particular). Fall back to the project web URL + ".git",
	// GitLab's canonical https clone URL, so a repo-bound bot triggered from an
	// issue comment still gets a non-empty RepoURL to clone.
	if p.CloneURL == "" && e.Project.WebURL != "" {
		p.CloneURL = e.Project.WebURL + ".git"
	}
	if e.MergeRequest != nil {
		p.MRIID = e.MergeRequest.IID
		p.MRState = e.MergeRequest.State
		p.SourceBranch = e.MergeRequest.SourceBranch
		p.TargetBranch = e.MergeRequest.TargetBranch
		p.MRTitle = e.MergeRequest.Title
		p.MRDesc = e.MergeRequest.Description
		p.MRURL = e.MergeRequest.URL
		p.HeadSHA = e.MergeRequest.LastCommit.ID
	}
	if e.Issue != nil {
		p.IssueIID = e.Issue.IID
		p.IssueTitle = e.Issue.Title
		p.IssueDesc = e.Issue.Description
		p.IssueURL = e.Issue.URL
		p.IssueState = e.Issue.State
	}
	// noteable_type lives on object_attributes. A note that targets a
	// MergeRequest OR an Issue is routable (the MR conversational flow / the
	// Issue /command-opens-MR flow); only a genuinely unroutable noteable
	// (Commit, Snippet, …) is an error so the handler filters it. An empty
	// noteable_type is tolerated (older payloads / synthetic fixtures).
	switch oa.NoteableType {
	case "", "MergeRequest", "Issue":
		return p, nil
	default:
		return p, fmt.Errorf("gitlab: note on %q is not routable (want MergeRequest or Issue)", oa.NoteableType)
	}
}

// IsMergeRequestNote reports whether this note is attached to an MR (the
// noteable the conversational /revi flow handles).
func (p ParsedNote) IsMergeRequestNote() bool { return p.MRIID != 0 && p.DiscussionID != "" }

// ThreadAnchor normalizes the note's own diff position to the launch shape
// the conversational bots declare. A reply note carries none — the thread
// root's position is the anchor, read from the discussion fetch
// (ThreadRootAnchor) — so this answers a real anchor only for the note that
// OPENED a diff thread (a `/revi <question>` typed as a fresh inline
// comment). GitLab names no side field: the image is which line is set.
func (p ParsedNote) ThreadAnchor() webhooks.ThreadAnchor {
	return positionAnchor(p.Position)
}

// positionAnchor maps a GitLab position onto the normalized ThreadAnchor:
// new_line set → the "new" side (old_line beside it when the line existed
// before); only old_line → the "old" side, a line the change removed.
// head_sha is the commit the position was taken on — the value a consumer
// compares against the revision it actually reads.
func positionAnchor(pos *NotePosition) webhooks.ThreadAnchor {
	if pos == nil {
		return webhooks.ThreadAnchor{}
	}
	a := webhooks.ThreadAnchor{Path: pos.NewPath, OldPath: pos.OldPath, Commit: pos.HeadSHA}
	switch {
	case pos.NewLine != nil && *pos.NewLine > 0:
		a.Side, a.Line = "new", *pos.NewLine
		if pos.OldLine != nil {
			a.OldLine = *pos.OldLine
		}
	case pos.OldLine != nil && *pos.OldLine > 0:
		a.Side, a.Line = "old", *pos.OldLine
	}
	return a
}

// IsIssueNote reports whether this note is attached to an Issue (the surface
// the /command-opens-MR flow handles). An MR note is never an issue note: when
// both blocks are somehow present the MR identity takes precedence so the
// conversational path keeps owning MR comments.
func (p ParsedNote) IsIssueNote() bool { return p.MRIID == 0 && p.IssueIID != 0 }

// Command extracts a leading slash-command from the note body, e.g.
// "/revi please re-review" → ("revi", "please re-review"). Returns
// ("", "") when the note does not start with a command. Delegates to
// webhooks.ParseSlashCommand so every comment surface shares one grammar
// (case-insensitive, tolerant of leading blank / quote-reply lines).
func (p ParsedNote) Command() (cmd, args string) {
	return webhooks.ParseSlashCommand(p.NoteBody)
}

// SubjectID is the stable per-note identifier used in delivery records +
// idempotency (one launch per note).
func (p ParsedNote) SubjectID() string {
	return "note:" + strconv.FormatInt(p.NoteID, 10)
}

// ParentSubjectID names the MERGE REQUEST a note hangs off ("mr:7"), or ""
// when the note sits on an issue. Distinct from SubjectID, which identifies
// the note itself and is what per-note idempotency keys on: a consumer
// asking "what did this merge request launch" — the closed-MR stop — cannot
// find a `/billy` run, or a converse reply, through "note:99". The
// GitHub/Forgejo twin is prforge.ParsedNote.ParentSubjectID.
// Anchored on the MR id ALONE, not IsMergeRequestNote: that predicate also
// demands a discussion id, which is what makes a note answerable in-thread —
// a routing question. Provenance is a different one, and a launch recorded
// against an MR must stay findable from it either way.
func (p ParsedNote) ParentSubjectID() string {
	if p.MRIID == 0 {
		return ""
	}
	return "mr:" + strconv.FormatInt(p.MRIID, 10)
}
