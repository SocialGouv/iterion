package mdcode

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The floors sit well under the corpus the walk serves (827 files at the
// time of writing, and every release only adds): a walk finding fewer is a
// green gate over an unread tree — and, under MDCODE_UPDATE_GOLDEN=1, a
// rewrite rewriteGolden refuses.
const (
	goldenPath  = "testdata/committed-reading.txt"
	corpusFloor = 700
	mdFloor     = 600
	yamlFloor   = 100
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
// whether that is the change's point or its regression. When it is the
// change's point — a new reading, or a committed file edited on purpose —
// the deliberate rewrite is spelled
//
//	MDCODE_UPDATE_GOLDEN=1 go test ./internal/mdcode/ -run TestTheCommittedCorpusKeepsItsReading -count=1 -v
//
// (-v because cmd/go swallows a PASSING package's output without it, and the
// rewrite's stderr announcement must show) — the same opt-in shape as pkg/dsl/author's golden, under a name this
// package owns so one variable cannot rewrite every golden in the tree. The
// release cut spells it itself: every release rewrites CHANGELOG.md and
// charts/iterion/Chart.yaml, so release-it regenerates the golden from its
// before:git:beforeRelease hook, inside the release commit
// (TestTheReleaseCutRegeneratesTheGolden pins the wiring). The rewrite goes
// through rewriteGolden, which refuses a walk under the floors: the flag
// cannot record a tree the walk stopped seeing. TestTheGoldenOnlyMovesOnPurpose
// is the witness that neither a flagless run nor a blind walk moves the
// golden.
func TestTheCommittedCorpusKeepsItsReading(t *testing.T) {
	var lines []string
	got := map[string]string{}
	var md, yaml int
	root := "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		h := fmt.Sprintf("%x %x", mh, ch)
		got[path] = h
		// WalkDir's traversal order is deterministic, so a deliberate
		// rewrite diffs line for line.
		lines = append(lines, h+" "+path)
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
	if len(lines) < corpusFloor || md < mdFloor || yaml < yamlFloor {
		t.Fatalf("the walk found %d files (%d .md, %d .yaml) — the corpus is not what the golden was written from", len(lines), md, yaml)
	}
	if _, err := rewriteGolden(goldenPath, lines, md, yaml); err != nil {
		t.Fatalf("MDCODE_UPDATE_GOLDEN=1 is set but the rewrite refused: %v", err)
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read the golden (MDCODE_UPDATE_GOLDEN=1 rewrites it after a deliberate change): %v", err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(golden)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 {
			t.Fatalf("golden line %q is not two hashes and a path", line)
		}
		want[parts[2]] = parts[0] + " " + parts[1]
	}
	for path, h := range got {
		if want[path] != h {
			t.Errorf("%s reads differently now (mask+code %s, golden %s) — the block phase changed a committed verdict; if that is the change's point, rewrite the golden deliberately: MDCODE_UPDATE_GOLDEN=1 go test ./internal/mdcode/ -run TestTheCommittedCorpusKeepsItsReading -count=1 -v", path, h, want[path])
		}
		delete(want, path)
	}
	if len(want) > 0 {
		t.Errorf("%d golden file(s) the walk no longer reaches; first: %s", len(want), keys(want))
	}
}

// rewriteGolden rewrites the corpus golden when — and only when — the
// operator opted in with MDCODE_UPDATE_GOLDEN=1, and reports whether it
// wrote. A corpus under the walk's floors is refused even under the flag: a
// walk that cannot see the tree must not be able to erase the record of what
// the tree read, which is what makes the update a decision rather than an
// accident. A write is announced on stderr — never a t.Logf line that -v
// alone would show; cmd/go still swallows a PASSING package's output without
// -v, so the documented command and the release hook carry it, and the hook
// follows the regeneration with a `git diff --stat` line that logs the move
// unconditionally — and the write lands through a rename, so an interrupted
// run leaves the old golden whole.
func rewriteGolden(path string, lines []string, md, yaml int) (bool, error) {
	if os.Getenv("MDCODE_UPDATE_GOLDEN") != "1" {
		return false, nil
	}
	if len(lines) < corpusFloor || md < mdFloor || yaml < yamlFloor {
		return false, fmt.Errorf("the walk found %d files (%d .md, %d .yaml), under the floor — the golden stays as written", len(lines), md, yaml)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".committed-reading-*.tmp")
	if err != nil {
		return false, err
	}
	// Deferred cleanup is a no-op once the rename below has consumed the
	// temporary name; before it, it is what keeps a failed write from
	// leaving a .tmp beside the golden.
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "MDCODE_UPDATE_GOLDEN=1: rewrote %s from the walk's %d files — review the diff like any committed change\n", path, len(lines))
	return true, nil
}

// The golden above pins every committed file the four readers serve, so the
// one thing that must never happen on its own is the golden MOVING. This is
// the witness that the rewrite stays deliberate: no flag, no write; a
// flagged walk that lost the tree, no write either — and the file on disk is
// byte for byte what it was in both cases. The write that does land is
// announced on stderr — a real stream write, not a t.Logf line -v alone
// would show.
func TestTheGoldenOnlyMovesOnPurpose(t *testing.T) {
	golden := filepath.Join(t.TempDir(), "committed-reading.txt")
	before := []byte("a b old.md\n")
	if err := os.WriteFile(golden, before, 0o644); err != nil {
		t.Fatal(err)
	}
	unchanged := func() {
		t.Helper()
		body, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(before) {
			t.Fatalf("the golden moved on its own: %q", body)
		}
	}
	var lines []string
	for i := 0; i < corpusFloor; i++ {
		lines = append(lines, fmt.Sprintf("h h %d.md", i))
	}
	for i := 0; i < yamlFloor; i++ {
		lines = append(lines, fmt.Sprintf("h h %d.yaml", i))
	}
	md, yaml := corpusFloor, yamlFloor

	// A flagless run never writes — even one that happens to run with a
	// caller's MDCODE_UPDATE_GOLDEN in its environment is pinned off here.
	t.Setenv("MDCODE_UPDATE_GOLDEN", "")
	wrote, err := rewriteGolden(golden, lines, md, yaml)
	if err != nil || wrote {
		t.Fatalf("flagless rewriteGolden = %v, %v — the golden moved without the opt-in", wrote, err)
	}
	unchanged()

	t.Setenv("MDCODE_UPDATE_GOLDEN", "1")
	wrote, err = rewriteGolden(golden, lines[:10], 10, 0)
	if err == nil || wrote {
		t.Fatalf("an under-floor walk rewrote the golden (wrote=%v, err=%v) — blindness recorded as the new truth", wrote, err)
	}
	unchanged()

	stderr := captureStderr(t, func() {
		wrote, err = rewriteGolden(golden, lines, md, yaml)
	})
	if err != nil || !wrote {
		t.Fatalf("the deliberate rewrite refused a full corpus: wrote=%v, err=%v", wrote, err)
	}
	if !strings.Contains(stderr, golden) {
		t.Fatalf("the rewrite moved the golden without a word on stderr (got %q)", stderr)
	}
	body, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != strings.Join(lines, "\n")+"\n" {
		t.Fatalf("the rewritten golden is not the walked corpus")
	}
}

// The release cut rewrites CHANGELOG.md and charts/iterion/Chart.yaml on
// every release, and both sit in the corpus above: without the hook, every
// release reopens the red this file exists to make deliberate. The wiring is
// under test the way internal/floorsalign pins its own hook line: the slot
// is release-it's before:git:beforeRelease, where the changelog is rendered
// and nothing is staged yet, so the regenerated golden lands INSIDE the
// release commit the git plugin stages right after.
func TestTheReleaseCutRegeneratesTheGolden(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", ".release-it.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	slot := strings.Index(string(src), "'before:git:beforeRelease'")
	hook := strings.Index(string(src), "MDCODE_UPDATE_GOLDEN=1 go test ./internal/mdcode/")
	if slot < 0 || hook < 0 || hook < slot {
		t.Fatal(".release-it.mjs no longer regenerates the committed-reading golden from before:git:beforeRelease — every release reopens the red on CHANGELOG.md and charts/iterion/Chart.yaml (#2600)")
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
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
