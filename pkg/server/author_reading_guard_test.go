package server

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/modtypes"
)

// The author document's reading — the YAML decode of pkg/dsl/author
// (readFirst, and decode, which author.Parse runs), whatever door reaches
// it: author.Parse, ReadOne, Comments, WriteKeepingComments, canon.Document
// over them — is sized for the CLI: at the read budget's ceiling it holds
// ~600 MiB of decoded YAML in one process, through any of those doors. No
// server function reads one, and this keeps it so on the module as the type
// checker resolves it: every function of the module whose code reaches the
// decode — calling a door, or a function that does, however many hops, a
// wrapper of canon.Document included — is a reader, and no reader is a
// function of pkg/server. The files outside the author package that
// reference a door directly are listed, with why: a new one is a decision,
// made here. What the static graph cannot follow — a call through an
// interface or a function value bound elsewhere — it does not claim.
//
// The server's doors answer for themselves: a save, a bundle push and a
// launch refuse an author document by name (it is no .bot); /api/files/open
// reads a file by its path as a .bot, and an author document opened there
// is read as a .bot source, never decoded as YAML.
func TestNoServerCodeReadsAnAuthorDocument(t *testing.T) {
	const (
		authorPkg     = "github.com/SocialGouv/iterion/pkg/dsl/author."
		readFirst     = authorPkg + "readFirst"
		decode        = authorPkg + "decode"
		canonDocument = "github.com/SocialGouv/iterion/pkg/dsl/canon.Document"
	)
	direct := map[string]string{
		"pkg/dsl/canon/document.go":                  "canon.Document: the CLI's `iterion fmt` of a document",
		"pkg/cli/fmt.go":                             "`iterion fmt` (canon.Document)",
		"pkg/cli/fmt_author.go":                      "`iterion fmt` of a document beside its .bot",
		"pkg/cli/validate_author.go":                 "`iterion validate` of a document",
		"pkg/dsl/author/internal/probecheck/main.go": "the author package's own probe program",
	}
	m, err := modtypes.Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	g := m.Graph()
	readers := g.Reaching(readFirst, decode)
	// The author package's own files read their own documents; its
	// internal probe program is a reader like any other.
	ownFile := func(rel string) bool {
		return strings.HasPrefix(rel, "pkg/dsl/author/") && !strings.HasPrefix(rel, "pkg/dsl/author/internal/")
	}
	// The doors: every function of the author package that reaches the
	// decode, and canon.Document, which reads through them.
	doors := map[string]bool{readFirst: true, decode: true, canonDocument: true}
	for fn := range readers {
		if ownFile(g.Where[fn]) {
			doors[fn] = true
		}
	}
	files := map[string]bool{}
	for from, tos := range g.Refs {
		if ownFile(g.Where[from]) {
			continue
		}
		for to := range tos {
			if doors[to] {
				files[g.Where[from]] = true
			}
		}
	}
	var sorted []string
	for rel := range files {
		sorted = append(sorted, rel)
	}
	sort.Strings(sorted)
	for _, rel := range sorted {
		if _, ok := direct[rel]; !ok {
			t.Errorf("%s references a door onto the author document's reading (a function of pkg/dsl/author that decodes its YAML, or canon.Document): unlisted — list it with why, or read the .bot", rel)
		}
	}
	for rel := range direct {
		if !files[rel] {
			t.Errorf("%s is listed but no longer references the author document's reading: drop it", rel)
		}
	}
	var offenders []string
	for fn := range readers {
		if strings.HasPrefix(g.Where[fn], "pkg/server/") {
			offenders = append(offenders, fn)
		}
	}
	sort.Strings(offenders)
	for _, fn := range offenders {
		chain := []string{fn}
		for at := fn; at != readFirst && at != decode && len(chain) < 20; {
			at = readers[at]
			chain = append(chain, at)
		}
		t.Errorf("%s (%s) reads an author document, whose reading is sized for the CLI: %s", fn, g.Where[fn], strings.Join(chain, " → "))
	}
}
