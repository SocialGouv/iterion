package cloudpublisher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The refusal of a bundle launch whose fragment does not parse cites the
// fragment by its unit-relative path, never by the snapshot's absolute one
// on THIS server: the error is answered to the client as the 400 body
// `launch: %v`, and the snapshot's directory is the server's layout, not
// the client's (#1934's root cut, on the publish path).
func TestMarshalIRFromSpecNamesABrokenFragmentWithoutTheSnapshotRoot(t *testing.T) {
	dir := t.TempDir()
	main := "import \"lib/nodes.bot\"\n\nworkflow main:\n  entry: done\n"
	for name, content := range map[string]string{
		"main.bot":      main,
		"lib/nodes.bot": "prompt p:\n  hi\n\nagent \n  model\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := marshalIRFromSpec("bots/probe/main.bot", main, dir)
	if err == nil {
		t.Fatal("the fixture no longer arms the case: the broken fragment serialised")
	}
	if !strings.Contains(err.Error(), "lib/nodes.bot:") {
		t.Errorf("the error does not cite the fragment by its relative path: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error discloses the snapshot's absolute path: %v", err)
	}
}

// The include-inlining refusal of a bundle launch names the include as the
// prompt wrote it, never the absolute path the server resolved it to.
func TestMarshalIRFromSpecNamesAMissingIncludeWithoutTheSnapshotRoot(t *testing.T) {
	dir := t.TempDir()
	main := "prompt p:\n  {{include \"nope.md\"}}\n\nworkflow main:\n  entry: done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := marshalIRFromSpec("bots/probe/main.bot", main, dir)
	if err == nil {
		t.Fatal("the fixture no longer arms the case: the missing include serialised")
	}
	if !strings.Contains(err.Error(), "nope.md") {
		t.Errorf("the error does not name the missing include: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error discloses the snapshot's absolute path: %v", err)
	}
}
