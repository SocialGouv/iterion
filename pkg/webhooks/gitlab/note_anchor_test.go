package gitlab

import (
	"encoding/json"
	"testing"
)

// The thread root's diff position: GitLab attaches a `position` object to
// the note that OPENS a diff thread and to no reply, so the arriving note
// carries it only when the note IS the root (a fresh inline comment), and
// the discussion fetch carries it on the root note (ThreadRootAnchor).
// These shapes pin what both normalize — and that a removed line reads as
// the old side, never as a current line.
func noteParsed(t *testing.T, body string) ParsedNote {
	t.Helper()
	p, err := ParseNote([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p
}

func TestParsedNoteThreadAnchor(t *testing.T) {
	p := noteParsed(t, `{
		"object_kind": "note",
		"user": {"id": 2, "username": "alice"},
		"project": {"id": 42, "path_with_namespace": "g/p", "git_http_url": "https://gitlab.example/g/p.git", "web_url": "https://gitlab.example/g/p"},
		"object_attributes": {
			"id": 99, "note": "/revi why here?", "noteable_type": "MergeRequest",
			"discussion_id": "d-1", "author_id": 2,
			"position": {
				"base_sha": "b1", "start_sha": "s1", "head_sha": "h1",
				"old_path": "pkg/x/y.go", "new_path": "pkg/x/y.go",
				"old_line": 40, "new_line": 42, "position_type": "text"
			}
		},
		"merge_request": {"iid": 7, "state": "opened", "source_branch": "f", "target_branch": "main", "url": "https://gitlab.example/g/p/-/merge_requests/7"}
	}`)
	a := p.ThreadAnchor()
	if a.Path != "pkg/x/y.go" || a.Line != 42 || a.Side != "new" || a.Commit != "h1" {
		t.Errorf("anchor = %+v, want the new-side line 42 on pkg/x/y.go at h1", a)
	}
	if a.OldLine != 40 || a.OldPath != "pkg/x/y.go" {
		t.Errorf("the paired pre-image line travels as the hint (40) and the path spelling too: %+v", a)
	}
}

func TestParsedNoteThreadAnchorRemovedLine(t *testing.T) {
	p := noteParsed(t, `{
		"object_kind": "note",
		"user": {"id": 2, "username": "alice"},
		"project": {"id": 42, "path_with_namespace": "g/p", "git_http_url": "https://gitlab.example/g/p.git"},
		"object_attributes": {
			"id": 99, "note": "this line is gone", "noteable_type": "MergeRequest",
			"discussion_id": "d-1",
			"position": {"base_sha": "b1", "start_sha": "s1", "head_sha": "h1",
				"old_path": "pkg/x/y.go", "new_path": "pkg/x/y.go",
				"old_line": 12, "position_type": "text"}
		},
		"merge_request": {"iid": 7, "state": "opened"}
	}`)
	a := p.ThreadAnchor()
	if a.Side != "old" || a.Line != 12 || a.OldLine != 0 {
		t.Errorf("a removed line is the old side with no separate hint: %+v", a)
	}
}

func TestParsedNoteThreadAnchorRename(t *testing.T) {
	p := noteParsed(t, `{
		"object_kind": "note",
		"user": {"id": 2, "username": "alice"},
		"project": {"id": 42, "path_with_namespace": "g/p", "git_http_url": "https://gitlab.example/g/p.git"},
		"object_attributes": {
			"id": 99, "note": "after the rename", "noteable_type": "MergeRequest",
			"discussion_id": "d-1",
			"position": {"base_sha": "b1", "start_sha": "s1", "head_sha": "h1",
				"old_path": "pkg/old/y.go", "new_path": "pkg/new/y.go",
				"new_line": 8, "position_type": "text"}
		},
		"merge_request": {"iid": 7, "state": "opened"}
	}`)
	a := p.ThreadAnchor()
	if a.Path != "pkg/new/y.go" || a.OldPath != "pkg/old/y.go" {
		t.Errorf("a rename travels as old_path + new_path: %+v", a)
	}
}

func TestParsedNoteThreadAnchorReplyCarriesNone(t *testing.T) {
	p := noteParsed(t, `{
		"object_kind": "note",
		"user": {"id": 2, "username": "alice"},
		"project": {"id": 42, "path_with_namespace": "g/p", "git_http_url": "https://gitlab.example/g/p.git"},
		"object_attributes": {"id": 100, "note": "a plain reply", "noteable_type": "MergeRequest", "discussion_id": "d-1"},
		"merge_request": {"iid": 7, "state": "opened"}
	}`)
	if p.ThreadAnchor().Set() {
		t.Errorf("a reply carries no position and must anchor nothing: %+v", p.ThreadAnchor())
	}
}

// ThreadRootAnchor reads the ROOT's position from the discussion fetch —
// the fetch the reply gate already makes, no second call. Only the root
// note carries one, so "the first" cannot depend on the fetch's order.
func TestThreadRootAnchor(t *testing.T) {
	root := &NotePosition{NewPath: "pkg/x/y.go", NewLine: int64p(42), HeadSHA: "h1"}
	notes := []DiscussionNote{
		{AuthorID: 1, Body: "the review comment", Position: root},
		{AuthorID: 2, Body: "the reply"},
	}
	a := ThreadRootAnchor(notes)
	if !a.Set() || a.Line != 42 || a.Commit != "h1" {
		t.Errorf("the root's position is the anchor: %+v", a)
	}
	if a := ThreadRootAnchor(nil); a.Set() {
		t.Errorf("no notes, no anchor: %+v", a)
	}
	// A general thread: no positioned note anywhere.
	if a := ThreadRootAnchor([]DiscussionNote{{AuthorID: 1, Body: "a note"}}); a.Set() {
		t.Errorf("a general thread anchors nothing: %+v", a)
	}
	// Order-independence in effect: only the root can carry a position, so
	// wherever it sits in the slice the answer is the same anchor.
	reversed := []DiscussionNote{
		{AuthorID: 2, Body: "the reply"},
		{AuthorID: 1, Body: "the review comment", Position: root},
	}
	if ThreadRootAnchor(reversed) != a {
		t.Errorf("the anchor must not depend on the slice order: %+v vs %+v", ThreadRootAnchor(reversed), a)
	}
}

// The Discussion API shape: each note carries its position, decoded.
func TestDiscussionParsesNotePositions(t *testing.T) {
	// Decoded through the same struct Discussion reads: the wire shape is
	// notes[].position.
	var d struct {
		Notes []struct {
			Body     string        `json:"body"`
			System   bool          `json:"system"`
			Position *NotePosition `json:"position"`
		} `json:"notes"`
	}
	body := `{"notes": [
		{"body": "root", "position": {"head_sha": "h1", "new_path": "a.go", "new_line": 5, "old_path": "a.go"}},
		{"body": "reply"}
	]}`
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Notes) != 2 || d.Notes[0].Position == nil || d.Notes[1].Position != nil {
		t.Fatalf("positions decode per note: %+v", d)
	}
	if d.Notes[0].Position.HeadSHA != "h1" || d.Notes[0].Position.NewLine == nil || *d.Notes[0].Position.NewLine != 5 {
		t.Errorf("the root's position decodes: %+v", d.Notes[0].Position)
	}
}

func int64p(n int64) *int64 { return &n }

// The root identity is the thread's FIRST note, not "first positioned": a
// GitLab that ever positioned a reply must not move the anchor to whatever
// order the fetch lists. And a system note never carries the diff anchor.
func TestThreadRootAnchorPrefersTheRootNote(t *testing.T) {
	root := &NotePosition{NewPath: "pkg/x/y.go", NewLine: int64p(42), HeadSHA: "h1"}
	reply := &NotePosition{NewPath: "pkg/x/y.go", NewLine: int64p(99), HeadSHA: "h2"}
	// The root is notes[0] (chronological API order); its position wins
	// over any later note's — the anchor is the root's identity, not
	// "first positioned in the slice".
	a := ThreadRootAnchor([]DiscussionNote{
		{AuthorID: 1, Body: "the root", Position: root},
		{AuthorID: 2, Body: "a reply", Position: reply},
	})
	if a.Line != 42 || a.Commit != "h1" {
		t.Errorf("the ROOT note's position wins, not a later positioned one: %+v", a)
	}
	// A system note never carries the diff anchor — not even from the
	// first slot: skip it and take the first non-system positioned note.
	sys := &NotePosition{NewPath: "pkg/sys.go", NewLine: int64p(7), HeadSHA: "h3"}
	a = ThreadRootAnchor([]DiscussionNote{
		{AuthorID: 1, Body: "closed the thread", System: true, Position: sys},
		{AuthorID: 1, Body: "the review comment", Position: root},
	})
	if a.Line != 42 || a.Commit != "h1" {
		t.Errorf("a system-positioned note is skipped: %+v", a)
	}
}
