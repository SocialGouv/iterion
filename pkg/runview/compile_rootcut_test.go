package runview

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// brokenUnit writes a bot in two files — main.bot importing lib/nodes.bot —
// under a fresh dir, with the fragment's text the caller names, and returns
// the dir.
func brokenUnit(t *testing.T, fragment string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	main := "import \"lib/nodes.bot\"\n\nworkflow x:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "nodes.bot"), []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRelLaunchErrorNamesTheRefusedRootCut: a unit rooted at the filesystem
// root gets no root cut (#2047), and the launch path serves error TEXTS —
// never the diagnostics list the E048 warning rides — so relLaunchError
// names the refusal in the body that keeps the absolute names, with the
// refusal's chain intact.
func TestRelLaunchErrorNamesTheRefusedRootCut(t *testing.T) {
	u := &unit.Unit{Root: "/", Main: "main.bot"}
	sentinel := errors.New("parse error: /main.bot:2:3: error [E002]: expected ] to close the list")
	err := relLaunchError("", "", u, sentinel)
	if !errors.Is(err, sentinel) {
		t.Errorf("the refusal's chain was broken: %v", err)
	}
	if !strings.Contains(err.Error(), "/main.bot") {
		t.Errorf("the name the cut cannot rewrite is gone: %v", err)
	}
	if !strings.Contains(err.Error(), "filesystem root") {
		t.Errorf("the refused cut is not named: %v", err)
	}
	if got := relLaunchError("", "", u, nil); got != nil {
		t.Errorf("a clean launch carries no note: %v", got)
	}
}

// TestCompileForLaunchNamesABrokenFragmentWithoutTheHostRoot: the compile
// behind Service.Launch — whose error the run console answers as the 400
// body `launch: %v` — cites a fragment the parser refused by its
// unit-relative path, never by the absolute one the loader parsed it under
// (#1934's root cut, applied to the launch path; the server's directory
// layout is not the client's to read).
func TestCompileForLaunchNamesABrokenFragmentWithoutTheHostRoot(t *testing.T) {
	dir := brokenUnit(t, "prompt p:\n  hi\n\nagent \n  model\n")
	_, _, _, err := compileForLaunch(filepath.Join(dir, "main.bot"), "", "")
	if err == nil {
		t.Fatal("the fixture no longer arms the case: the broken fragment compiled")
	}
	if !strings.Contains(err.Error(), "lib/nodes.bot:") {
		t.Errorf("the error does not cite the fragment by its relative path: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error discloses the server's directory layout: %v", err)
	}
}

// TestCompileForLaunchNamesAMissingIncludeWithoutTheHostRoot: the include of
// a prompt resolves beside the file's ABSOLUTE path on disk, so the stat
// error riding the compile error named the host root until the cut.
func TestCompileForLaunchNamesAMissingIncludeWithoutTheHostRoot(t *testing.T) {
	dir := t.TempDir()
	main := "prompt p:\n  {{include \"nope.md\"}}\n\nworkflow x:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := compileForLaunch(filepath.Join(dir, "main.bot"), "", "")
	if err == nil {
		t.Fatal("the fixture no longer arms the case: the missing include compiled")
	}
	if !strings.Contains(err.Error(), "nope.md") {
		t.Errorf("the error does not name the missing include: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error discloses the server's directory layout: %v", err)
	}
}

// The stored-bundle refusal — `open stored bot bundle: %w` when the
// snapshot's own manifest does not decode — names the snapshot's
// directory, which the unit's root never covers (the launch's file path
// lies elsewhere: the materialized inline copy under the store). It is
// the one refusal only the bundleDir root cut reaches; dropping that root
// turns this witness red.
func TestCompileForLaunchNamesAStoredBundleThatDoesNotOpenWithoutTheHostRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow x:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The launch's file path: the materialized inline source, nowhere
	// near the snapshot.
	materialized := filepath.Join(t.TempDir(), "92b6803b1f32-main.bot")
	_, _, _, err := compileForLaunch(materialized, "workflow x:\n  entry: done\n", dir)
	if err == nil {
		t.Fatal("the fixture no longer arms the case: the corrupt snapshot opened")
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("the error does not name the manifest: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error discloses the snapshot's absolute path: %v", err)
	}
}
