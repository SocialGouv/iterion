package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotPreservesBundleCollection(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string][]byte{
		"parent/main.bot": []byte("parent-v1"), "parent/devbox.json": []byte("{}"),
		"parent/skills/read.md": []byte("skill-v1"), "child/extend.bot": []byte("child-v1"),
		"child/main.bot": []byte("child-main"), "child/helper": {0, 1, 255},
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := &Snapshot{Root: "parent"}
	for _, name := range []string{"parent", "child"} {
		if err := s.AddDir(name, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	body, digest, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child/extend.bot"), []byte("child-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	frozen, err := DecodeSnapshot(body, digest)
	if err != nil {
		t.Fatal(err)
	}
	root, cleanup, err := frozen.Materialize()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for rel, want := range map[string]string{"devbox.json": "{}", "skills/read.md": "skill-v1"} {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || string(got) != want {
			t.Fatalf("resource %s lost: %q %v", rel, got, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "../child/extend.bot"))
	if err != nil || string(got) != "child-v1" {
		t.Fatalf("child changed: %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(root, "../child/helper"))
	if err != nil || string(got) != string([]byte{0, 1, 255}) {
		t.Fatalf("binary changed: %v %v", got, err)
	}
	info, err := os.Stat(filepath.Join(root, "../child/helper"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("executable bit lost: %v", err)
	}
	if _, err := DecodeSnapshot(append(body, ' '), digest); err == nil {
		t.Fatal("modified payload accepted")
	}
	if _, err := ResolveSnapshotSource("../../escape.bot", root, filepath.Dir(root)); err == nil {
		t.Fatal("escaped collection")
	}
	if _, err := ResolveSnapshotSource("../missing/main.bot", root, filepath.Dir(root)); err == nil {
		t.Fatal("missing child accepted")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "leak.bot"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSnapshotSource("alias/leak.bot", root, filepath.Dir(root)); err == nil {
		t.Fatal("followed a directory symlink outside the collection")
	}
}

func TestSnapshotRejectsUnsafeOrPartialTrees(t *testing.T) {
	for _, path := range []string{"../secret", "/etc/passwd", "root/../../secret", "root/x\\y", "root//x", "root"} {
		s := &Snapshot{Root: "root", Files: map[string]SnapshotFile{"root/main.bot": {Content: []byte("main")}, path: {Content: []byte("bad")}}}
		if _, _, err := s.Encode(); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	dir := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "main.bot")); err != nil {
		t.Fatal(err)
	}
	if err := (&Snapshot{Root: "root"}).AddDir("root", dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink: %v", err)
	}
	if _, _, err := (&Snapshot{Root: "absent", Files: map[string]SnapshotFile{"other/main.bot": {}}}).Encode(); err == nil {
		t.Fatal("missing parent accepted")
	}
}
