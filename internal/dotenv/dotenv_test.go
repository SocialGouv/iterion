package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

// parse writes the content to a temp file and returns the entries the one
// shared reader returns: the file's bytes in, its entries out.
func parse(t *testing.T, content string) []Entry {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return entries
}

// expect checks entries against name→value pairs (and the single-quote
// flag, in a second table).
func expect(t *testing.T, entries []Entry, want map[string]string, singleQuoted map[string]bool) {
	t.Helper()
	if got := len(entries); got != len(want) && got != len(singleQuoted) {
		t.Fatalf("got %d entries, want %d", got, len(want))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		w, ok := want[e.Key]
		if !ok {
			t.Errorf("unexpected entry %s=%q", e.Key, e.Value)
			continue
		}
		if e.Value != w {
			t.Errorf("%s = %q, want %q", e.Key, e.Value, w)
		}
		if sq := singleQuoted[e.Key]; e.SingleQuoted != sq {
			t.Errorf("%s SingleQuoted = %v, want %v", e.Key, e.SingleQuoted, sq)
		}
		seen[e.Key] = true
	}
	if len(seen) != len(want) {
		for k := range want {
			if !seen[k] {
				t.Errorf("missing entry %s", k)
			}
		}
	}
}

// The one primitive, tested directly: the bytes each lane feeds it, the
// entries each lane plants. Before the H-10 round-6 review, this package —
// the shared primitive of the CLI's applyDotEnv, the studio's Snapshot and
// the desktop's env-file loader — carried no test at all; the review's
// rendering table was exactly what these tests would have held.
func TestTheSharedParseReadsWhatTheLanesFeedIt(t *testing.T) {
	entries := parse(t, `# a leading comment
FIRST=ok
export SECOND=also ok
QUOTED="dquoted value"
DQESC="a\"b"
DQC="v1" # trailing comment dropped
SINGLE='sq value'
SQC='sq value' # trailing comment dropped
SQRIDE='it''s'
UNQUOTED=a\z b
HASHED=abc #123
MULTI="a # b"
EMPTY=
SPACED_KEY = spaced value
`)
	expect(t, entries,
		map[string]string{
			"FIRST":      "ok",
			"SECOND":     "also ok",
			"QUOTED":     "dquoted value",
			"DQESC":      `a\"b`,
			"DQC":        "v1",
			"SINGLE":     "sq value",
			"SQC":        "sq value",
			"SQRIDE":     `it''s`,
			"UNQUOTED":   `a\z b`,
			"HASHED":     "abc #123",
			"MULTI":      "a # b",
			"EMPTY":      "",
			"SPACED_KEY": "spaced value",
		},
		map[string]bool{
			"SINGLE": true,
			"SQC":    true,
			// The outer strip the parser has always done on a line that
			// ends in the quote it opened with (`'it''s'` reads `it''s`).
			"SQRIDE": true,
		})
}

// The lines that are not assignments are skipped, not executed, not
// planted: comments, bare words, names no shell could export (the marker
// cannot carry them; Windows would fold them).
func TestTheSharedParseSkipsWhatNoLaneMayPlant(t *testing.T) {
	entries := parse(t, `# comment line
a bare command
NOT AN ASSIGNMENT
9LEAD=leading digit
HAS.DOT=no
export
FIRST=ok
`)
	if len(entries) != 1 || entries[0].Key != "FIRST" || entries[0].Value != "ok" {
		t.Fatalf("entries = %#v, want only FIRST=ok", entries)
	}
}
