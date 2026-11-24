package prforge

import "testing"

// The thread's diff position rides the pull_request_review_comment payload,
// and a review-thread REPLY carries the same anchor its root does — the
// whole thread hangs off one diff spot. These shapes pin what
// ParsedReviewComment.ThreadAnchor normalizes: a fresh right-side comment,
// a comment on a REMOVED line (left side), an OUTDATED comment (line null,
// original_line kept — the pre-image hint that must never read as a
// current line), and a Forgejo/Gitea payload that names the file and
// nothing else.
func TestParseReviewCommentThreadAnchor(t *testing.T) {
	anchor := func(body string) (int64, int64, string, string, string, string) {
		t.Helper()
		p, err := ParseReviewComment([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		a := p.ThreadAnchor()
		if p.CommentPath != "pkg/x/y.go" {
			t.Errorf("CommentPath = %q, want the parsed file the thread anchors to", p.CommentPath)
		}
		return a.Line, a.OldLine, a.Side, a.Commit, a.Path, a.OldPath
	}

	t.Run("fresh right-side comment", func(t *testing.T) {
		line, oldLine, side, commit, path, oldPath := anchor(`{
			"comment": {"id": 9002, "in_reply_to_id": 9001, "body": "why?",
				"path": "pkg/x/y.go", "line": 42, "original_line": 40,
				"side": "RIGHT", "original_side": "RIGHT",
				"commit_id": "c0ffee1", "original_commit_id": "dead123",
				"user": {"login": "alice"}},
			"pull_request": {"number": 7, "state": "open", "head": {"sha": "x", "ref": "f"}, "base": {"ref": "main"}},
			"repository": {"id": 42, "full_name": "acme/widgets"}
		}`)
		if line != 42 || side != "new" || commit != "c0ffee1" || path != "pkg/x/y.go" {
			t.Errorf("anchor = line %d side %q commit %q path %q; want 42/new/c0ffee1/pkg/x/y.go", line, side, commit, path)
		}
		if oldLine != 40 || oldPath != "" {
			t.Errorf("the pre-image hint travels (40), the old path does not (GitHub names no rename): oldLine %d oldPath %q", oldLine, oldPath)
		}
	})

	t.Run("a removed line: the LEFT side normalizes to old", func(t *testing.T) {
		line, _, side, _, _, _ := anchor(`{
			"comment": {"id": 5, "body": "this line is gone",
				"path": "pkg/x/y.go", "line": 12, "side": "LEFT",
				"commit_id": "c0ffee1", "user": {"login": "alice"}},
			"pull_request": {"number": 7, "state": "open", "head": {"sha": "x", "ref": "f"}, "base": {"ref": "main"}},
			"repository": {"id": 42, "full_name": "acme/widgets"}
		}`)
		if side != "old" || line != 12 {
			t.Errorf("a left-side comment is the old side: side %q line %d", side, line)
		}
	})

	t.Run("an outdated comment: line null, original_line kept but never a current line", func(t *testing.T) {
		line, oldLine, side, _, _, _ := anchor(`{
			"comment": {"id": 6, "body": "why here?",
				"path": "pkg/x/y.go", "line": null, "original_line": 12,
				"side": "RIGHT", "commit_id": "c0ffee1", "user": {"login": "alice"}},
			"pull_request": {"number": 7, "state": "open", "head": {"sha": "x", "ref": "f"}, "base": {"ref": "main"}},
			"repository": {"id": 42, "full_name": "acme/widgets"}
		}`)
		if line != 0 {
			t.Errorf("a null line must stay absent, not become a guessed one: %d", line)
		}
		if oldLine != 12 || side != "new" {
			t.Errorf("the original line travels as the pre-image hint (12) beside the named side (new): oldLine %d side %q", oldLine, side)
		}
	})

	t.Run("a Forgejo payload that names only the file degrades to the path", func(t *testing.T) {
		line, _, side, _, path, _ := anchor(`{
			"comment": {"id": 7, "body": "note", "path": "pkg/x/y.go", "user": {"login": "alice"}},
			"pull_request": {"number": 7, "state": "open", "head": {"sha": "x", "ref": "f"}, "base": {"ref": "main"}},
			"repository": {"id": 42, "full_name": "acme/widgets"}
		}`)
		if path != "pkg/x/y.go" || line != 0 || side != "" {
			t.Errorf("path survives, the missing fields degrade to empty: path %q line %d side %q", path, line, side)
		}
	})

	t.Run("a sideless payload's bare line is a pre-image hint, not a current line", func(t *testing.T) {
		// A payload that carries a number but no image it sits on cannot
		// name a current line; the value must demote to the hint, which no
		// consumer reads as current.
		line, oldLine, side, _, _, _ := anchor(`{
			"comment": {"id": 8, "body": "note", "path": "pkg/x/y.go", "line": 7,
				"commit_id": "c0ffee1", "user": {"login": "alice"}},
			"pull_request": {"number": 7, "state": "open", "head": {"sha": "x", "ref": "f"}, "base": {"ref": "main"}},
			"repository": {"id": 42, "full_name": "acme/widgets"}
		}`)
		if line != 0 || oldLine != 7 || side != "" {
			t.Errorf("an unknown image demotes the line to the hint: line %d oldLine %d side %q", line, oldLine, side)
		}
	})

	t.Run("a payload with no position at all anchors nothing", func(t *testing.T) {
		p, err := ParseReviewComment([]byte(`{
			"comment": {"id": 9, "body": "note", "user": {"login": "alice"}},
			"pull_request": {"number": 7, "state": "open", "head": {"sha": "x", "ref": "f"}, "base": {"ref": "main"}},
			"repository": {"id": 42, "full_name": "acme/widgets"}
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if p.ThreadAnchor().Set() {
			t.Errorf("no position in the payload must anchor nothing: %+v", p.ThreadAnchor())
		}
	})
}
