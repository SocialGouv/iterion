package mdcode_test

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/corpusreading"
)

// The four first-party readers of this package — the catalogue generator
// (pkg/botregistry), the docs map (pkg/repomap), the link checker
// (internal/docsguard) and the artefact gate (bots) — all walk the committed
// tree through the same two calls: Mask, and FenceScanner.Code over Lines.
// Whatever this package reads differently, they all read differently, so a
// change to the reading must be decided on the corpus the readers serve.
// This is that witness: every tracked .md, .yaml and .yml outside vendor/,
// hashed under the reading — the docs and ADRs, and the bots' own skills,
// presets and manifest.yaml under bots/. The walk is internal/corpusreading's,
// the one the release cut and `task corpus:reading` write the golden with:
// it reads only what the index carries, so a build or a local draft run
// before the test does not change what the corpus pins.
//
// The golden moves deliberately, never on its own: a moved hash means the
// readers' verdict on a committed file changed, and the review decides
// whether that is the change's point or its regression. A change that edits a
// pinned file carries the golden update in its own diff (`task
// corpus:reading -- --write`, then review the moved lines); the release
// commit rewrites it itself for the CHANGELOG.md and chart edits it ships. A
// walk that finds too few files is a green gate over an unread tree — the
// floor is the walk's own count at the time of writing, well under the
// tracked total.
func TestTheCommittedCorpusKeepsItsReading(t *testing.T) {
	want := readGolden(t)
	if len(want) < 800 {
		t.Fatalf("only %d file(s) in the golden — the walk is not reaching the tree", len(want))
	}
	entries, err := corpusreading.Read(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var md, yaml int
	for _, e := range entries {
		if got := e.Reading(); want[e.Path] != got {
			t.Errorf("%s reads differently now (mask+code %s, golden %s) — the block phase changed a committed verdict; if that is the change's point, rewrite the golden deliberately: task corpus:reading -- --write", e.Path, got, want[e.Path])
		}
		delete(want, e.Path)
		if path.Ext(e.Path) == ".md" {
			md++
		} else {
			yaml++
		}
	}
	if len(want) > 0 {
		t.Errorf("%d golden file(s) the walk no longer reaches; first: %s", len(want), keys(want))
	}
	if md < 600 || yaml < 100 {
		t.Errorf("the walk found %d .md and %d .yaml — the corpus is not what the golden was written from", md, yaml)
	}
}

// The release cut rewrites the golden over the whole corpus, so the bytes
// the generator writes for an unchanged corpus must be the committed bytes:
// otherwise every cut would also reorder or reformat the lines of files it
// never touched, and the release commit would stop saying which pins it moved.
func TestTheGoldenIsWhatTheGeneratorWrites(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(corpusreading.Golden)))
	if err != nil {
		t.Fatalf("read the golden: %v", err)
	}
	entries, err := corpusreading.Read(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	if written := corpusreading.Format(entries); !bytes.Equal(written, committed) {
		w, c := strings.Split(string(written), "\n"), strings.Split(string(committed), "\n")
		i := 0
		for i < len(w) && i < len(c) && w[i] == c[i] {
			i++
		}
		var wl, cl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(c) {
			cl = c[i]
		}
		t.Fatalf("the generator's golden differs from the committed one at line %d:\n  writes: %q\n  golden: %q\nrewrite it with `task corpus:reading -- --write` so a cut's commit moves only the lines of the files it edits", i+1, wl, cl)
	}
}

func readGolden(t *testing.T) map[string]string {
	t.Helper()
	golden, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(corpusreading.Golden)))
	if err != nil {
		t.Fatalf("read the golden: %v", err)
	}
	want, err := corpusreading.Parse(golden)
	if err != nil {
		t.Fatalf("parse the golden: %v", err)
	}
	return want
}

func keys(m map[string]string) string {
	var b strings.Builder
	first := true
	for k := range m {
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(k)
	}
	return b.String()
}
