package catalogclaims

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// botUnitSource is the text of a catalog bot's unit — main first, then the
// fragments its imports reach. A file that is not a unit (a bot in one file,
// a skill, a manifest, a host source) reads byte for byte as os.ReadFile does.
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
// library under root/bots — the texts a human reads to decide, and the texts
// a model is handed as mission. It is bots' list of the same name, rooted.
func pagesAReaderAndAModelRead(t *testing.T, root string) []string {
	t.Helper()
	botsDir := filepath.Join(root, "bots")
	var pages []string
	for _, pattern := range []string{"*/README.md", "*/skills/*.md", "*/*.bot", "*/lib/*.bot"} {
		matched, err := filepath.Glob(filepath.Join(botsDir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(matched) == 0 {
			t.Fatalf("pattern %q matched nothing under %s — the glob is stale", pattern, botsDir)
		}
		pages = append(pages, matched...)
	}
	// A floor, because a scanner that reads nothing passes everything.
	if len(pages) < 200 {
		t.Fatalf("scanned %d pages, want >= 200 — the globs no longer cover the class", len(pages))
	}
	return pages
}

func readCopilotContractFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := botUnitSource(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func copilotContractSection(t *testing.T, source, start, end string) string {
	t.Helper()
	startAt := strings.Index(source, start)
	if startAt < 0 {
		t.Fatalf("could not find contract owner %q", start)
	}
	endAt := strings.Index(source[startAt+len(start):], end)
	if endAt < 0 {
		t.Fatalf("could not find end %q for contract owner %q", end, start)
	}
	return source[startAt : startAt+len(start)+endAt]
}
