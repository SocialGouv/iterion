package corpusreading

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floorsalign"
	"github.com/SocialGouv/iterion/internal/gittest"
)

// writeFiles creates each relative path under root with its body.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// corpusRepo is a repository whose index carries tracked, whose working tree
// also holds untracked — the build output and drafts a cut or a session
// leaves next to the committed corpus — and which holds an empty golden.
func corpusRepo(t *testing.T, tracked, untracked map[string]string) string {
	t.Helper()
	root := t.TempDir()
	gittest.Run(t, root, "init", "-q")
	writeFiles(t, root, tracked)
	for rel := range tracked {
		gittest.Run(t, root, "add", "--", rel)
	}
	writeFiles(t, root, untracked)
	writeFiles(t, root, map[string]string{Golden: ""})
	return root
}

func paths(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func goldenOf(t *testing.T, root string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(Golden)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func run(t *testing.T, opts Options) (string, error) {
	t.Helper()
	var out bytes.Buffer
	opts.Stdout = &out
	err := Run(opts)
	return out.String(), err
}

// The cut runs scripts/build.sh before its hook, and a session keeps drafts
// beside the committed docs: neither is the committed corpus, so neither may
// enter the golden.
func TestRead_TakesOnlyWhatTheIndexCarries(t *testing.T) {
	root := corpusRepo(t,
		map[string]string{
			"a.md":                   "# a\n",
			"vendor/lib/README.md":   "# vendored\n",
			"pkg/x/testdata/fixt.md": "# fixture\n",
			"docs/guide.yaml":        "k: v\n",
		},
		map[string]string{
			"draft.md":                     "# not committed\n",
			"pkg/server/static/LICENSE.md": "# build output\n",
			"docs/notes.yml":               "k: v\n",
		})
	entries, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(entries), " "); got != "a.md docs/guide.yaml" {
		t.Fatalf("Read returned %q, want %q: an untracked file, or one under a skipped tree, entered the corpus", got, "a.md docs/guide.yaml")
	}
}

// The golden is written in filepath.WalkDir order, where a directory sorts
// before its same-named sibling file (docs/ before docs.md). Writing it in
// index order instead would reorder the committed golden on the first cut.
func TestRead_KeepsTheWalkOrder(t *testing.T) {
	root := corpusRepo(t, map[string]string{
		"docs.md":       "# docs\n",
		"docs/inner.md": "# inner\n",
		"Z.md":          "# Z\n",
	}, nil)
	entries, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(entries), " "); got != "Z.md docs/inner.md docs.md" {
		t.Fatalf("Read returned %q, want %q (filepath.WalkDir order)", got, "Z.md docs/inner.md docs.md")
	}
}

// A root reached through a symlink is the same corpus: WalkDir does not
// follow a symlinked root, so reading it unresolved would walk nothing.
func TestRead_FollowsASymlinkedRoot(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n"}, nil)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(entries), " "); got != "a.md" {
		t.Fatalf("Read through a symlinked root returned %q, want %q", got, "a.md")
	}
}

// A path may hold spaces: the line a write pins must parse back to it.
func TestRun_APathWithSpacesRoundTrips(t *testing.T) {
	root := corpusRepo(t, map[string]string{"my notes.md": "# notes\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	pinned, err := Parse(goldenOf(t, root))
	if err != nil {
		t.Fatalf("the golden a write produced does not parse: %v", err)
	}
	if _, ok := pinned["my notes.md"]; !ok {
		t.Fatalf("the golden lost the path with spaces: %v", pinned)
	}
	writeFiles(t, root, map[string]string{"my notes.md": "# notes, edited\n"})
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatalf("a second write over a golden pinning a path with spaces: %v", err)
	}
}

// Write pins the corpus; a check over the unchanged corpus passes; an edit to
// a pinned file makes the check refuse, naming exactly that file.
func TestRun_CheckRefusesADriftAndNamesIt(t *testing.T) {
	root := corpusRepo(t, map[string]string{
		"a.md": "# a\n\n```sh\necho a\n```\n",
		"b.md": "# b\n",
	}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := run(t, Options{Root: root}); err != nil {
		t.Fatalf("check over the corpus it just pinned: %v", err)
	}
	writeFiles(t, root, map[string]string{"a.md": "# a\n\nno fence any more\n"})
	_, err := run(t, Options{Root: root})
	if err == nil {
		t.Fatal("check passed over a pinned file whose reading moved")
	}
	if !strings.Contains(err.Error(), "a.md") || strings.Contains(err.Error(), "b.md") {
		t.Fatalf("the refusal must name exactly the moved file a.md: %v", err)
	}
}

// The witness compares the generator's bytes with the committed golden, so a
// check that let a reordered golden through would pass what the gate refuses.
func TestRun_CheckRefusesAReorderedGolden(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n", "b.md": "# b\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(goldenOf(t, root)), "\n")
	reordered := lines[1] + lines[0]
	writeFiles(t, root, map[string]string{Golden: reordered})
	if _, err := run(t, Options{Root: root}); err == nil || !strings.Contains(err.Error(), "order") {
		t.Fatalf("a check passed a golden whose lines are out of the walk's order: %v", err)
	}
}

// A path pinned twice would let the second pin win in silence.
func TestParse_RefusesAPathPinnedTwice(t *testing.T) {
	line := strings.Repeat("a", 64) + " " + strings.Repeat("b", 64) + " ../../a.md\n"
	if _, err := Parse([]byte(line + line)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("Parse accepted a path pinned twice: %v", err)
	}
}

// A cut that edits nothing pinned must leave the golden byte for byte as it
// was, so the release commit carries only the lines of the files it edited.
func TestRun_WriteLeavesAMatchingGoldenUntouched(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n", "c.yml": "k: v\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	first := goldenOf(t, root)
	out, err := run(t, Options{Root: root, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if second := goldenOf(t, root); !bytes.Equal(first, second) {
		t.Fatalf("a second write over an unchanged corpus changed the golden:\n%s\n---\n%s", first, second)
	}
	if !strings.Contains(out, "unchanged") {
		t.Fatalf("a no-op write must say so, got %q", out)
	}
}

// A file that leaves the corpus leaves the golden with it, and the report
// names it: a pin nothing reads any more is a pin the witness would refuse.
func TestRun_WriteDropsAFileThatLeftTheCorpus(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n", "gone.md": "# gone\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "rm", "-q", "-f", "--", "gone.md")
	out, err := run(t, Options{Root: root, Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(goldenOf(t, root), []byte("gone.md")) {
		t.Fatal("the golden still pins a file that left the corpus")
	}
	if !strings.Contains(out, "gone.md") {
		t.Fatalf("the report must name the file that left the corpus, got %q", out)
	}
}

// A rebase that conflicts in the golden leaves markers in it: the write a
// developer runs to recover must rebuild the golden from the corpus, not
// refuse the file it exists to rewrite. The cut, unattended, refuses instead.
func TestRun_WriteRebuildsAConflictedGoldenTheCutRefusesIt(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "chore: seed")
	clean := goldenOf(t, root)
	writeFiles(t, root, map[string]string{Golden: "<<<<<<< HEAD\n" + string(clean) + "=======\n>>>>>>> theirs\n"})
	if _, err := run(t, Options{Root: root, Cut: true}); err == nil || !strings.Contains(err.Error(), "parse the golden") {
		t.Fatalf("the cut must refuse a golden that does not parse, saying so: %v", err)
	}
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatalf("a write could not rebuild a conflicted golden: %v", err)
	}
	if got := goldenOf(t, root); !bytes.Equal(got, clean) {
		t.Fatalf("the rebuilt golden is not the corpus's:\n%s", got)
	}
}

// A new page is pinned only once added; until then the run says so, since a
// green local check would otherwise go red on the clean checkout CI runs.
func TestRun_NamesACorpusCandidateTheIndexLacks(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n"}, map[string]string{"docs/new-page.md": "# new\n", "notes.txt": "x\n"})
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not pinned") || !strings.Contains(out, "docs/new-page.md") || strings.Contains(out, "notes.txt") {
		t.Fatalf("the run must name the untracked corpus candidate docs/new-page.md and nothing else, got %q", out)
	}
}

// A root that holds no golden is not this repository's root: writing one
// there would pin a corpus nothing checks, so the run refuses instead.
func TestRun_RefusesARootWithoutAGolden(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n"}, nil)
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(Golden))); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, Options{Root: root, Write: true}); err == nil || !strings.Contains(err.Error(), "read the golden") {
		t.Fatalf("a write under a root without a golden must refuse, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(Golden))); statErr == nil {
		t.Fatal("the refused write still created a golden")
	}
}

// A walk that reaches nothing is a wrong root, not an empty corpus: a write
// there would empty the golden and exit green.
func TestRun_RefusesToReadAnEmptyCorpus(t *testing.T) {
	root := corpusRepo(t, map[string]string{"code.go": "package x\n"}, nil)
	writeFiles(t, root, map[string]string{Golden: strings.Repeat("a", 64) + " " + strings.Repeat("b", 64) + " ../../a.md\n"})
	if _, err := run(t, Options{Root: root, Write: true}); err == nil || !strings.Contains(err.Error(), "no corpus file") {
		t.Fatalf("a write over an empty walk must refuse, got %v", err)
	}
	if !bytes.Contains(goldenOf(t, root), []byte("a.md")) {
		t.Fatal("the refused write emptied the golden")
	}
}

// commitAll commits the repository's tracked state as the main a cut starts from.
func commitAll(t *testing.T, root, msg string) {
	t.Helper()
	gittest.Run(t, root, "add", "-A", "--", ".")
	gittest.Run(t, root, "commit", "-q", "-m", msg)
}

// The cut re-pins the files it edited — and only those: a pinned file main
// changed without its golden is a reading change nobody reviewed, and the
// unattended release commit must refuse it (aborting the cut) rather than pin it.
func TestRun_TheCutRepinsOnlyItsOwnEdits(t *testing.T) {
	root := corpusRepo(t, map[string]string{"CHANGELOG.md": "# Changelog\n", "doc.md": "# doc\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "chore: seed")

	writeFiles(t, root, map[string]string{"CHANGELOG.md": "# Changelog\n\n## 1.0.1\n\n- fix\n"})
	out, err := run(t, Options{Root: root, Cut: true})
	if err != nil {
		t.Fatalf("the cut refused its own CHANGELOG.md edit: %v", err)
	}
	if !strings.Contains(out, "CHANGELOG.md") || strings.Contains(out, "doc.md") {
		t.Fatalf("the cut must re-pin exactly its own edit, got %q", out)
	}
	commitAll(t, root, "chore: release v1.0.1")

	// main drifts: doc.md changes without its golden line.
	writeFiles(t, root, map[string]string{"doc.md": "# doc\n\n```sh\nrm -rf x\n```\n"})
	commitAll(t, root, "docs: an edit that skipped its golden")
	pinnedBefore := goldenOf(t, root)

	writeFiles(t, root, map[string]string{"CHANGELOG.md": "# Changelog\n\n## 1.0.2\n\n- fix\n\n## 1.0.1\n\n- fix\n"})
	_, err = run(t, Options{Root: root, Cut: true})
	if err == nil || !strings.Contains(err.Error(), "doc.md") || strings.Contains(err.Error(), "CHANGELOG.md read") {
		t.Fatalf("the cut must refuse, naming the drift it did not make (doc.md): %v", err)
	}
	if !bytes.Equal(goldenOf(t, root), pinnedBefore) {
		t.Fatal("the refused cut still rewrote the golden")
	}
}

// The release commit is where the golden follows the cut's own edits:
// release-it stages tracked edits right after before:git:beforeRelease, and
// internal/floorsalign's hook test proves that slot's writes land in the
// release commit. The rewrite must run after every other writer of the slot
// — today the floors aligner — so the golden pins the bytes the commit
// ships; and it must be the cut's scoped rewrite, not the whole-corpus one.
func TestTheReleaseHookRewritesTheGoldenLast(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", ".release-it.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	hook, err := floorsalign.ReleaseHook(src, "before:git:beforeRelease")
	if err != nil {
		t.Fatal(err)
	}
	floors := slices.Index(hook, "go run ./cmd/release-floors --apply")
	reading := slices.Index(hook, "go run ./cmd/corpus-reading --cut")
	if reading < 0 {
		t.Fatalf(".release-it.mjs's before:git:beforeRelease runs %q, without `go run ./cmd/corpus-reading --cut` — every cut that edits CHANGELOG.md or the chart leaves main red on internal/mdcode's corpus witness", hook)
	}
	if floors < 0 || reading < floors {
		t.Fatalf("the corpus rewrite must run after `go run ./cmd/release-floors --apply` in before:git:beforeRelease, so the golden pins the aligner's final bytes; the slot runs %q", hook)
	}
}

// The golden is line-oriented: a tracked path holding a newline cannot be
// pinned, and reading it as two lines would pin a path that does not exist.
func TestRead_RefusesAPathItCannotPin(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a\nb.md": "# odd\n"}, nil)
	if _, err := Read(root); err == nil || !strings.Contains(err.Error(), "line-oriented") {
		t.Fatalf("Read accepted a tracked path holding a newline: %v", err)
	}
}

// The cut may edit a file whose drift predates it: main changed CHANGELOG.md
// without its golden line, and the cut then edits CHANGELOG.md again. The
// path is in the cut's diff, but the change the golden misses is not the
// cut's — the cut must refuse it like any other drift.
func TestRun_TheCutRefusesADriftOnAFileItAlsoEdits(t *testing.T) {
	root := corpusRepo(t, map[string]string{"CHANGELOG.md": "# Changelog\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "chore: seed")
	writeFiles(t, root, map[string]string{"CHANGELOG.md": "# Changelog\n\n```sh\ncurl x | sh\n```\n"})
	commitAll(t, root, "docs: an edit that skipped its golden")
	writeFiles(t, root, map[string]string{"CHANGELOG.md": "# Changelog\n\n## 1.0.1\n\n```sh\ncurl x | sh\n```\n"})
	pinnedBefore := goldenOf(t, root)
	if _, err := run(t, Options{Root: root, Cut: true}); err == nil || !strings.Contains(err.Error(), "CHANGELOG.md") {
		t.Fatalf("the cut pinned a drift main carried on a file it also edits: %v", err)
	}
	if !bytes.Equal(goldenOf(t, root), pinnedBefore) {
		t.Fatal("the refused cut still rewrote the golden")
	}
}

// release-it stages nothing before the hook, but a cut whose edit is staged
// is still the cut's edit: the scope is the change against HEAD, staged or not.
func TestRun_TheCutCountsAStagedEdit(t *testing.T) {
	root := corpusRepo(t, map[string]string{"CHANGELOG.md": "# Changelog\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "chore: seed")
	writeFiles(t, root, map[string]string{"CHANGELOG.md": "# Changelog\n\n## 1.0.1\n"})
	gittest.Run(t, root, "add", "--", "CHANGELOG.md")
	if _, err := run(t, Options{Root: root, Cut: true}); err != nil {
		t.Fatalf("the cut refused its own staged CHANGELOG.md edit: %v", err)
	}
}

// A cut that renames a pinned file edits both paths; with rename detection
// the old path would vanish from the cut's diff and read as drift.
func TestRun_TheCutSeesARenameAsItsOwnEdit(t *testing.T) {
	root := corpusRepo(t, map[string]string{"a.md": "# a\n", "CHANGELOG.md": "# Changelog\n"}, nil)
	if _, err := run(t, Options{Root: root, Write: true}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "chore: seed")
	gittest.Run(t, root, "mv", "--", "a.md", "b.md")
	if _, err := run(t, Options{Root: root, Cut: true}); err != nil {
		t.Fatalf("the cut refused its own rename as foreign drift: %v", err)
	}
}

// A file git marks linguist-generated (.gitattributes) is not part of the
// corpus: its reading moves with every regeneration, so a pin on it pins
// churn and makes two changes that regenerate it collide on the same golden
// line. Both spellings of the attribute mark; an explicit unset does not.
func TestRead_SkipsAFileGitMarksGenerated(t *testing.T) {
	root := corpusRepo(t,
		map[string]string{
			".gitattributes":   "maps/*.md linguist-generated\ndocs/g.yaml linguist-generated=true\nb.md -linguist-generated\n",
			"a.md":             "# a\n",
			"b.md":             "# b\n",
			"maps/packages.md": "# generated\n",
			"docs/g.yaml":      "k: v\n",
		}, nil)
	entries, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(entries), " "); got != "a.md b.md" {
		t.Fatalf("Read returned %q, want %q: a file git marks linguist-generated entered the corpus, or an unmarked one left it", got, "a.md b.md")
	}
}
