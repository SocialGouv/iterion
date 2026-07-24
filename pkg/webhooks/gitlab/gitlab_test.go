package gitlab

import "testing"

const mrOpenPayload = `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {
    "id": 42,
    "path_with_namespace": "acme/widgets",
    "web_url": "https://gitlab.com/acme/widgets",
    "git_http_url": "https://gitlab.com/acme/widgets.git"
  },
  "object_attributes": {
    "iid": 7,
    "action": "open",
    "source_branch": "feature/x",
    "target_branch": "main",
    "title": "Add X",
    "description": "Implements X",
    "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": { "id": "abc123" }
  },
  "labels": [{ "title": "review" }, { "title": "" }]
}`

func TestParseMergeRequest(t *testing.T) {
	p, err := ParseMergeRequest([]byte(mrOpenPayload))
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectID != 42 || p.ProjectPath != "acme/widgets" || p.CloneURL != "https://gitlab.com/acme/widgets.git" {
		t.Fatalf("project: %+v", p)
	}
	if p.MRIID != 7 || p.SourceBranch != "feature/x" || p.TargetBranch != "main" || p.HeadSHA != "abc123" {
		t.Fatalf("mr: %+v", p)
	}
	if p.MRURL != "https://gitlab.com/acme/widgets/-/merge_requests/7" || p.SubjectID() != "mr:7" {
		t.Fatalf("url/subject: %+v", p)
	}
	if len(p.Labels) != 1 || p.Labels[0] != "review" {
		t.Fatalf("labels (empty filtered): %v", p.Labels)
	}
	if !p.IsReviewable() {
		t.Fatal("open MR should be reviewable")
	}
}

func TestParseMergeRequest_RejectsNonMR(t *testing.T) {
	if _, err := ParseMergeRequest([]byte(`{"object_kind":"push"}`)); err == nil {
		t.Fatal("non-merge_request should error")
	}
	if _, err := ParseMergeRequest([]byte(`{bad`)); err == nil {
		t.Fatal("malformed json should error")
	}
}

func TestIsReviewable(t *testing.T) {
	cases := []struct {
		action             string
		draft, becameReady bool
		want               bool
	}{
		{"open", false, false, true},
		{"reopen", false, false, true},
		{"update", false, false, false}, // push no longer auto-triggers (re-review is on-demand via /revi)
		{"close", false, false, false},
		{"approved", false, false, false},
		// A currently-draft MR never auto-triggers, whatever the action.
		{"open", true, false, false},
		{"reopen", true, false, false},
		// The draft→ready transition (update clearing draft) IS the trigger —
		// GitLab has no dedicated ready action, so it rides `update`.
		{"update", false, true, true},
		// Draft cleared but still marked draft (defensive) must not trigger.
		{"update", true, true, false},
	}
	for _, c := range cases {
		p := Parsed{Action: c.action, Draft: c.draft, BecameReady: c.becameReady}
		if p.IsReviewable() != c.want {
			t.Errorf("action=%q draft=%v becameReady=%v => %v want %v", c.action, c.draft, c.becameReady, p.IsReviewable(), c.want)
		}
	}
}

func TestIsSynchronize(t *testing.T) {
	cases := []struct {
		action string
		oldRev string
		draft  bool
		want   bool
	}{
		{"update", "abc123", false, true}, // a push to the source branch
		{"update", "", false, false},      // metadata-only update, no code change
		{"open", "abc123", false, false},  // open is not a sync
		{"update", "abc123", true, false}, // a draft push never re-reviews
	}
	for _, c := range cases {
		p := Parsed{Action: c.action, OldRev: c.oldRev, Draft: c.draft}
		if got := p.IsSynchronize(); got != c.want {
			t.Errorf("action=%q oldrev=%q draft=%v => %v want %v", c.action, c.oldRev, c.draft, got, c.want)
		}
	}
}

// mrDraftOpenPayload is a draft MR opened — must be filtered (never
// auto-launched) even though the action is "open".
const mrDraftOpenPayload = `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {
    "iid": 8, "action": "open", "source_branch": "wip/x", "target_branch": "main",
    "title": "Draft: Add X", "url": "https://gitlab.com/acme/widgets/-/merge_requests/8",
    "draft": true, "work_in_progress": true, "last_commit": {"id": "abc123"}
  }
}`

// mrReadyPayload is the draft→ready transition: an `update` whose
// changes.draft went true→false. This IS the auto-trigger.
const mrReadyPayload = `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {
    "iid": 8, "action": "update", "source_branch": "wip/x", "target_branch": "main",
    "title": "Add X", "url": "https://gitlab.com/acme/widgets/-/merge_requests/8",
    "draft": false, "work_in_progress": false, "last_commit": {"id": "abc123"}
  },
  "changes": {"draft": {"previous": true, "current": false}}
}`

func TestParseMergeRequest_Draft(t *testing.T) {
	draft, err := ParseMergeRequest([]byte(mrDraftOpenPayload))
	if err != nil {
		t.Fatal(err)
	}
	if !draft.Draft {
		t.Error("draft MR must parse Draft=true")
	}
	if draft.IsReviewable() {
		t.Error("draft MR (action=open) must NOT be auto-reviewable")
	}

	ready, err := ParseMergeRequest([]byte(mrReadyPayload))
	if err != nil {
		t.Fatal(err)
	}
	if ready.Draft {
		t.Error("ready MR must parse Draft=false")
	}
	if !ready.BecameReady {
		t.Error("draft→ready update must set BecameReady")
	}
	if !ready.IsReviewable() {
		t.Error("draft→ready transition MUST be auto-reviewable")
	}
}

const noteRevi = `{
  "object_kind": "note",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "user": {"username": "alice"},
  "object_attributes": {"id": 99, "note": "/revi", "noteable_type": "MergeRequest", "author_id": 1},
  "merge_request": {"iid": 7, "state": "opened", "source_branch": "feature/x", "target_branch": "main",
    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": {"id": "headsha"}}
}`

// Parser-level note tests (TestParseNote, TestParseNote_NonMR,
// TestNoteCommand) live in note_test.go next to the parser. Here we
// cover the /revi specialization the re-review trigger consumes: MR
// state + command grammar through IsReviewCommand, against the same
// payload shape the handler sees.
func TestParseNote_ReviewCommandEndToEnd(t *testing.T) {
	p, err := ParseNote([]byte(noteRevi))
	if err != nil {
		t.Fatal(err)
	}
	if p.MRState != "opened" || p.AuthorUsername != "alice" {
		t.Fatalf("note: %+v", p)
	}
	if !p.IsReviewCommand() {
		t.Fatal("bare /revi on an open MR should be a review command")
	}
}

func TestIsReviewCommand(t *testing.T) {
	base := ParsedNote{MRIID: 7, MRState: "opened"}
	cases := []struct {
		note string
		want bool
	}{
		{"/revi", true},
		{"/revi focus=security", true},
		{"   /revi   ", true}, // surrounding whitespace tolerated
		{"please run /revi", false},
		{"/revia", false},               // longer token; must NOT match
		{"/REVI", true},                 // Command() is case-insensitive by design
		{"> /revi quoted\n/revi", true}, // quote-reply prefix skipped (Command grammar)
		{"> some quoted context\nhi", false},
		{"", false},
		{"hi", false},
	}
	for _, c := range cases {
		p := base
		p.NoteBody = c.note
		if got := p.IsReviewCommand(); got != c.want {
			t.Errorf("note=%q => %v want %v", c.note, got, c.want)
		}
	}
	// closed MR is filtered even with the exact command
	closed := ParsedNote{MRIID: 7, MRState: "closed", NoteBody: "/revi"}
	if closed.IsReviewCommand() {
		t.Fatal("closed MR must filter /revi")
	}
	// non-MR note (no MR attached — commit/issue/snippet) is filtered
	issue := ParsedNote{MRState: "opened", NoteBody: "/revi"}
	if issue.IsReviewCommand() {
		t.Fatal("non-MR note must filter")
	}
}

// Allowlist matching tests live in pkg/webhooks/match_test.go (the
// canonical webhooks.MatchEvent + MatchProject are exercised there with
// every provider's default kinds).
