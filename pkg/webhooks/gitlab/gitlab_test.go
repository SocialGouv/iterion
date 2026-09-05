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

// The "Re-request review" button arrives as an `update` whose
// changes.reviewers.current stamps re_requested on the targeted reviewer
// (gitlab-org/gitlab!205274); adding a reviewer shows as current − previous.
// Both are the request-a-review gesture ReviewRequestedFrom answers for.
func TestParseMergeRequest_ReviewerChanges(t *testing.T) {
	payload := `{
	  "object_kind": "merge_request",
	  "user": {"username": "alice"},
	  "project": {"id": 42, "path_with_namespace": "acme/widgets"},
	  "object_attributes": {"iid": 7, "action": "update", "updated_at": "2026-09-01 10:00:00 UTC",
	    "url": "https://gitlab.com/acme/widgets/-/merge_requests/7", "last_commit": {"id": "abc123"}},
	  "changes": {"reviewers": {
	    "previous": [{"id": 12, "username": "carol"}],
	    "current": [{"id": 12, "username": "carol"}, {"id": 575, "username": "iterion-bot", "re_requested": true}]
	  }}
	}`
	p, err := ParseMergeRequest([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ReRequestedReviewers) != 1 || p.ReRequestedReviewers[0] != "iterion-bot" {
		t.Fatalf("re-requested: %v", p.ReRequestedReviewers)
	}
	if len(p.AddedReviewers) != 1 || p.AddedReviewers[0] != "iterion-bot" {
		t.Fatalf("added (current − previous): %v", p.AddedReviewers)
	}
	if p.UpdatedAt != "2026-09-01 10:00:00 UTC" {
		t.Fatalf("updated_at: %q", p.UpdatedAt)
	}
	if !p.ReviewRequestedFrom("iterion-bot") || !p.ReviewRequestedFrom("ITERION-BOT") {
		t.Fatal("ReviewRequestedFrom must match the targeted reviewer (case-insensitively)")
	}
	if p.ReviewRequestedFrom("carol") {
		t.Fatal("an untouched pre-existing reviewer is not being asked for a review")
	}

	// Older GitLab without the re_requested attribute: adding the reviewer
	// is the only expressible form of the gesture — it must still match.
	older := `{
	  "object_kind": "merge_request",
	  "project": {"id": 42, "path_with_namespace": "acme/widgets"},
	  "object_attributes": {"iid": 7, "action": "update", "last_commit": {"id": "abc123"}},
	  "changes": {"reviewers": {"previous": [], "current": [{"id": 575, "username": "iterion-bot"}]}}
	}`
	p2, err := ParseMergeRequest([]byte(older))
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.ReRequestedReviewers) != 0 {
		t.Fatalf("no re_requested attr → none: %v", p2.ReRequestedReviewers)
	}
	if !p2.ReviewRequestedFrom("iterion-bot") {
		t.Fatal("adding the bot as reviewer is the same gesture on older GitLab")
	}
	// And an update with no reviewer change at all never matches.
	p3, err := ParseMergeRequest([]byte(mrOpenPayload))
	if err != nil {
		t.Fatal(err)
	}
	if p3.ReviewRequestedFrom("iterion-bot") {
		t.Fatal("no changes.reviewers → no review request")
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
// TestNoteCommand) live in note_test.go next to the parser. Here we pin the
// end-to-end shape the handler consumes: an MR note's `/revi` parses to the
// generic command grammar (cmd="revi", no args) against the same payload
// shape the handler sees — routing on it (bare → review-pr, with a
// question → revi-converse) is now the GENERIC webhooks.ResolveCommandRoute
// registry (pkg/server), not a GitLab-specific predicate here.
func TestParseNote_ReviewCommandEndToEnd(t *testing.T) {
	p, err := ParseNote([]byte(noteRevi))
	if err != nil {
		t.Fatal(err)
	}
	if p.MRState != "opened" || p.AuthorUsername != "alice" {
		t.Fatalf("note: %+v", p)
	}
	if cmd, args := p.Command(); cmd != "revi" || args != "" {
		t.Fatalf("bare /revi on an open MR should parse to the revi command with no args, got cmd=%q args=%q", cmd, args)
	}
}

// Allowlist matching tests live in pkg/webhooks/match_test.go (the
// canonical webhooks.MatchEvent + MatchProject are exercised there with
// every provider's default kinds).
