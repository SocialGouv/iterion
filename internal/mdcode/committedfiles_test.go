package mdcode

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four first-party readers of this package — the catalogue generator
// (pkg/botregistry), the docs map (pkg/repomap), the link checker
// (internal/docsguard) and the artefact gate (bots) — all walk the committed
// tree through the same two calls: Mask, and FenceScanner.Code over Lines.
// Whatever this package reads differently, they all read differently, so a
// change to the reading must be decided on the corpus the readers serve.
// This is that witness: every tracked .md, .yaml and .yml outside vendor/,
// hashed under the reading — the docs and ADRs, and the bots' own skills,
// presets and manifest.yaml under bots/. The dispatcher catalogue's sync
// output (pkg/cli/templates/dispatch_bots/) is gitignored build output
// spliced from bots/ at build time, so the walk skips it like vendor/: a
// build run before the test must not change what the corpus pins.
//
// The golden must not move here on its own: a moved hash means the four
// readers changed verdict on a committed file, and the review decides
// whether that is the change's point or its regression. A walk that finds
// too few files is a green gate over an unread tree — the floor is the
// walk's own count at the time of writing, well under the tracked total.
func TestTheCommittedCorpusKeepsItsReading(t *testing.T) {
	golden, err := os.ReadFile("testdata/committed-reading.txt")
	if err != nil {
		t.Fatalf("read the golden: %v", err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(golden)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 {
			t.Fatalf("golden line %q is not two hashes and a path", line)
		}
		want[parts[2]] = parts[0] + " " + parts[1]
	}
	if len(want) < 800 {
		t.Fatalf("only %d file(s) in the golden — the walk is not reaching the tree", len(want))
	}
	var md, yaml int
	root := "../.."
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".repos", ".works", ".claude", ".iterion", "testdata", "dispatch_bots":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".md", ".yaml", ".yml":
		default:
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var f FenceScanner
		var code []byte
		for _, line := range Lines(string(body)) {
			if f.Code(line) {
				code = append(code, '1')
			} else {
				code = append(code, '0')
			}
		}
		mh := sha256.Sum256([]byte(Mask(string(body))))
		ch := sha256.Sum256(code)
		got := fmt.Sprintf("%x %x", mh, ch)
		if want[path] != got {
			t.Errorf("%s reads differently now (mask+code %s, golden %s) — the block phase changed a committed verdict; if that is the change's point, rewrite the golden deliberately", path, got, want[path])
		}
		delete(want, path)
		switch filepath.Ext(path) {
		case ".md":
			md++
		default:
			yaml++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) > 0 {
		t.Errorf("%d golden file(s) the walk no longer reaches; first: %s", len(want), keys(want))
	}
	if md < 600 || yaml < 100 {
		t.Errorf("the walk found %d .md and %d .yaml — the corpus is not what the golden was written from", md, yaml)
	}
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
