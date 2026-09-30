package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
