package bots

import (
	"bytes"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// parseBotUnit parses a catalog bot as its UNIT — the file and the fragments
// its imports reach (`import "lib/x.bot"`) — and returns what parser.Parse
// returns for a single file, so a test that read main.bot alone reads the
// whole program. A bot in one file parses exactly as before.
func parseBotUnit(path string) *parser.ParseResult {
	u := unittest.Whole(unit.LoadDir(path))
	return &parser.ParseResult{File: u.Merged, Diagnostics: u.Diagnostics}
}

// botUnitSource is the text of a catalog bot's unit — main first, then the
// fragments its imports reach — for the tests that read the DSL as text: a
// clause in a prompt, a `skills:` list, a tool's command body. A file that
// is not a unit (a bot in one file, a skill, a manifest) reads byte for
// byte as os.ReadFile does.
func botUnitSource(path string) ([]byte, error) {
	u := unittest.Whole(unit.LoadDir(path))
	if len(u.Files) <= 1 {
		return os.ReadFile(path)
	}
	var b bytes.Buffer
	for _, f := range u.Files {
		b.Write(f.Source)
		if !bytes.HasSuffix(f.Source, []byte("\n")) {
			b.WriteByte('\n')
		}
	}
	return b.Bytes(), nil
}

// pagesAReaderAndAModelRead is the page class the prose guards scan: every
// bundle README, every verify skill, every bot source and every shared node
// library — the texts a human reads to decide, and the texts a model is
// handed as mission.
//
// `*/*.bot`, not `*/main.bot`: a bundle may carry sibling entrypoints
// (golden-master ships extend.bot, reanchor.bot and sync-harness.bot) and may
// have no main.bot at all (smoke ships board_smoke.bot), so naming main.bot
// makes four shipped sources invisible to every class claim.
//
// One list for every guard over this class. A copied list drifts, and a claim
// that travels by copy-paste is exactly what these guards exist to catch.
func pagesAReaderAndAModelRead(t *testing.T) []string {
	t.Helper()
	var pages []string
	for _, pattern := range []string{"*/README.md", "*/skills/*.md", "*/*.bot", "*/lib/*.bot"} {
		matched, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matched) == 0 {
			t.Fatalf("pattern %q matched nothing — the glob is stale", pattern)
		}
		pages = append(pages, matched...)
	}
	// A floor, because a scanner that reads nothing passes everything.
	if len(pages) < 200 {
		t.Fatalf("scanned %d pages, want >= 200 — the globs no longer cover the class", len(pages))
	}
	return pages
}
