package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

const copyUnitMain = "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: a\n  a -> done\n"
const copyUnitNodes = "agent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"d\"\n"

func unitBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"skills", "lib"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(copyUnitMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "nodes.bot"), []byte(copyUnitNodes), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A studio run records as FilePath the store's materialised copy of its
// main and its bundle as BundlePath; a resume compiles that copy against
// the bundle. A bot in several files is read beside its fragments, which
// live beside the ORIGINAL: the copy compiles as the bundle's main, and
// what the compile read is handed to the launcher, main first.
func TestACopyOfTheMainOutsideItsBundleCompilesAsTheBundlesMain(t *testing.T) {
	dir := unitBundle(t)
	copyPath := filepath.Join(t.TempDir(), "a1b2c3d4e5f6-main.bot")
	if err := os.WriteFile(copyPath, []byte(copyUnitMain), 0o644); err != nil {
		t.Fatal(err)
	}
	// The launch: source inline + the bundle dir.
	wf, cs, b, err := compileForLaunch(copyPath, copyUnitMain, dir)
	if err != nil || wf == nil || b == nil {
		t.Fatalf("launch of the copy: %v", err)
	}
	if cs.Main != "main.bot" || len(cs.Files) != 2 || cs.Files["lib/nodes.bot"] != copyUnitNodes {
		t.Fatalf("the compile did not hand its files over: %+v", cs)
	}
	// The resume: the copy's path alone + the recorded bundle dir.
	_, rcs, _, err := compileForLaunch(copyPath, "", dir)
	if err != nil {
		t.Fatalf("resume from the copy: %v", err)
	}
	if rcs.Hash != cs.Hash || len(rcs.Files) != 2 {
		t.Fatalf("the resume read another unit: hash %s vs %s, %d files", rcs.Hash, cs.Hash, len(rcs.Files))
	}
	// The bundle's own main hashes the same: the copy IS that main.
	opened, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, want, err := CompileBundleWorkflow(opened.IterPath, opened); err != nil || want != cs.Hash {
		t.Fatalf("bundle main: hash %s err %v, want %s", want, err, cs.Hash)
	}
	// A child INSIDE a bundle is its own unit, read beside itself.
	child := filepath.Join(dir, "children")
	if err := os.MkdirAll(filepath.Join(child, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "kid.bot"), []byte("import \"lib/k.bot\"\n\nworkflow k:\n  entry: b\n  b -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "lib", "k.bot"), []byte("agent b:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"k\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, kcs, err := CompileBundleWorkflow(filepath.Join(child, "kid.bot"), opened); err != nil || kcs == "" {
		t.Fatalf("a child inside the bundle: %v", err)
	}
}

// What a rewind, a node diff or an export reads is the source as it is
// now: for a run whose FilePath is a copy outside its bundle, the bundle's
// main — beside its fragments — not the frozen copy.
func TestResolveWorkflowPath_ReadsTheBundlesMainForACopyOutsideIt(t *testing.T) {
	dir := unitBundle(t)
	copyPath := filepath.Join(t.TempDir(), "a1b2c3d4e5f6-main.bot")
	if err := os.WriteFile(copyPath, []byte(copyUnitMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveWorkflowPath(&store.Run{FilePath: copyPath, BundlePath: dir}); got != filepath.Join(dir, "main.bot") {
		t.Fatalf("got %q, want the bundle's main", got)
	}
	// A file inside its bundle, or a run with no bundle, reads as named.
	inside := filepath.Join(dir, "main.bot")
	if got := resolveWorkflowPath(&store.Run{FilePath: inside, BundlePath: dir}); got != inside {
		t.Fatalf("inside: got %q", got)
	}
	if got := resolveWorkflowPath(&store.Run{FilePath: copyPath}); got != copyPath {
		t.Fatalf("no bundle: got %q", got)
	}
	// A bundle path that no longer exists (a pod's) changes nothing.
	if got := resolveWorkflowPath(&store.Run{FilePath: copyPath, BundlePath: filepath.Join(dir, "gone")}); got != copyPath {
		t.Fatalf("gone bundle: got %q", got)
	}
	// A copy of a COMPANION workflow, should a launch ever materialise one,
	// is not the main and never reads as it.
	companion := filepath.Join(filepath.Dir(copyPath), "a1b2c3d4e5f6-reanchor.bot")
	if err := os.WriteFile(companion, []byte("workflow r:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveWorkflowPath(&store.Run{FilePath: companion, BundlePath: dir}); got != companion {
		t.Fatalf("a companion's copy was redirected to %q", got)
	}
}

// A relative FilePath — a catalog run, a subbot child under a relative
// parent — is never "outside" its bundle: the redirect to the bundle's main
// is for the store's absolute copy, and a child workflow keeps its own path.
func TestResolveWorkflowPath_KeepsARelativeChildPath(t *testing.T) {
	dir := unitBundle(t)
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "workflows", "child.bot")
	if err := os.WriteFile(child, []byte("workflow c:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(mustGetwd(t), child)
	if err != nil {
		t.Skipf("no relative path from the working directory: %v", err)
	}
	if got := resolveWorkflowPath(&store.Run{FilePath: rel, BundlePath: dir}); got != rel {
		t.Fatalf("a relative child path was redirected to %q", got)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// A file with a lib/ of its own beside it is a unit of its own wherever its
// bundle is: only a copy with nothing beside it reads as the bundle's main.
func TestAFileWithItsOwnFragmentsIsItsOwnUnit(t *testing.T) {
	dir := unitBundle(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(filepath.Join(other, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "main.bot"), []byte(copyUnitMain), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := "agent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"FOREIGN\"\n"
	if err := os.WriteFile(filepath.Join(other, "lib", "nodes.bot"), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	_, cs, _, err := compileForLaunch(filepath.Join(other, "main.bot"), "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if cs.Files["lib/nodes.bot"] != foreign {
		t.Fatalf("a file with its own lib/ read the bundle's fragments: %q", cs.Files["lib/nodes.bot"])
	}
	// The bundle reached through a symlinked directory is still the bundle.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !insideDir(filepath.Join(link, "main.bot"), dir) || !insideDir(filepath.Join(dir, "main.bot"), link) {
		t.Fatal("a path under a symlinked bundle directory reads as outside it")
	}
}
