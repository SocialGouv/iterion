package supervise

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewestTranscript(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		if _, _, ok := newestTranscript(filepath.Join(t.TempDir(), "nope")); ok {
			t.Error("missing dir must report no transcript")
		}
	})

	t.Run("empty dir", func(t *testing.T) {
		if _, _, ok := newestTranscript(t.TempDir()); ok {
			t.Error("empty dir must report no transcript")
		}
	})

	t.Run("newest jsonl wins; dirs and foreign files ignored", func(t *testing.T) {
		dir := t.TempDir()
		older := filepath.Join(dir, "old-session.jsonl")
		newer := filepath.Join(dir, "new-session.jsonl")
		for _, p := range []string{older, newer} {
			if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// Distractors: the subagents subdir and a non-jsonl file, both newer.
		if err := os.MkdirAll(filepath.Join(dir, "subagents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(older, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newer, now, now); err != nil {
			t.Fatal(err)
		}

		id, path, ok := newestTranscript(dir)
		if !ok {
			t.Fatal("no transcript found")
		}
		if id != "new-session" || path != newer {
			t.Errorf("newest = (%q, %q); want new-session", id, path)
		}
	})
}

func TestIsExistingDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"dir", dir, true},
		{"regular file", file, false},
		{"missing", filepath.Join(dir, "nope"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExistingDir(tc.in); got != tc.want {
				t.Errorf("isExistingDir(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolveClaudeSession(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	cwd := t.TempDir()
	key := ClaudeProjectKey(cwd)
	projDir := filepath.Join(configDir, "projects", key)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projDir, "abc-123.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("empty arg resolves cwd's newest transcript", func(t *testing.T) {
		sess, err := ResolveClaudeSession("", cwd)
		if err != nil {
			t.Fatalf("ResolveClaudeSession: %v", err)
		}
		if sess.Cwd != cwd || sess.ProjectKey != key {
			t.Errorf("sess = %+v; want cwd/key resolved", sess)
		}
		if sess.SessionID != "abc-123" || sess.TranscriptPath != transcript {
			t.Errorf("session = (%q, %q); want abc-123", sess.SessionID, sess.TranscriptPath)
		}
	})

	t.Run("directory arg wins over cwd", func(t *testing.T) {
		other := t.TempDir()
		sess, err := ResolveClaudeSession(cwd, other)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Cwd != cwd || sess.SessionID != "abc-123" {
			t.Errorf("sess = %+v; want the arg directory's session", sess)
		}
	})

	t.Run("session-id arg found by scanning project dirs", func(t *testing.T) {
		sess, err := ResolveClaudeSession("abc-123", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if sess.ProjectKey != key || sess.SessionID != "abc-123" || sess.TranscriptPath != transcript {
			t.Errorf("sess = %+v; want scan hit in %q", sess, key)
		}
		// Current behavior: the id-scan form leaves Cwd empty.
		if sess.Cwd != "" {
			t.Errorf("Cwd = %q; scan form currently leaves it empty", sess.Cwd)
		}
	})

	t.Run("cwd without transcripts resolves keys but no session", func(t *testing.T) {
		bare := t.TempDir()
		sess, err := ResolveClaudeSession("", bare)
		if err != nil {
			t.Fatal(err)
		}
		if sess.ProjectKey != ClaudeProjectKey(bare) {
			t.Errorf("ProjectKey = %q", sess.ProjectKey)
		}
		if sess.SessionID != "" || sess.TranscriptPath != "" {
			t.Errorf("session = (%q, %q); want unresolved", sess.SessionID, sess.TranscriptPath)
		}
	})

	// QUIRK (characterized): an arg that is neither an existing directory
	// nor a known session id is silently DROPPED — the session resolves
	// against cwd, not against the not-yet-existing path the caller named
	// (the code comment claims the latter).
	t.Run("unknown non-dir arg falls back to cwd", func(t *testing.T) {
		sess, err := ResolveClaudeSession("/definitely/not/a/dir", cwd)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Cwd != cwd {
			t.Errorf("Cwd = %q; current behavior resolves cwd, ignoring the arg", sess.Cwd)
		}
	})
}
