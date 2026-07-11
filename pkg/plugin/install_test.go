package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallWithLocalDir(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	src := t.TempDir()
	writeFileT(t, filepath.Join(src, ManifestFile), "name: local-demo\ncontributes:\n  skills: [skills/a.md]\n")
	writeFileT(t, filepath.Join(src, "skills", "a.md"), "# a\n")

	name, err := InstallWith(context.Background(), InstallOptions{Source: src})
	if err != nil {
		t.Fatalf("InstallWith: %v", err)
	}
	if name != "local-demo" {
		t.Fatalf("installed name = %q, want local-demo", name)
	}
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("local-demo")
	if !ok {
		t.Fatal("installed plugin not loaded")
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "skills", "a.md")); err != nil {
		t.Errorf("installed skill file missing: %v", err)
	}
}

func TestInstallWithSubpath(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	src := t.TempDir()
	writeFileT(t, filepath.Join(src, "plugins", "sub-demo", ManifestFile),
		"name: sub-demo\ncontributes:\n  skills: [skills/b.md]\n")
	writeFileT(t, filepath.Join(src, "plugins", "sub-demo", "skills", "b.md"), "# b\n")

	name, err := InstallWith(context.Background(), InstallOptions{Source: src, Subpath: "plugins/sub-demo"})
	if err != nil {
		t.Fatalf("InstallWith subpath: %v", err)
	}
	if name != "sub-demo" {
		t.Fatalf("installed name = %q, want sub-demo", name)
	}
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := reg.Get("sub-demo"); !ok {
		t.Fatal("subpath-installed plugin not loaded")
	}
}

func TestInstallWithRejectsTraversalAndLocalRef(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	src := t.TempDir()
	for _, sub := range []string{"../evil", "/abs", "a/../../evil"} {
		if _, err := InstallWith(context.Background(), InstallOptions{Source: src, Subpath: sub}); err == nil {
			t.Errorf("subpath %q accepted, want traversal rejection", sub)
		}
	}
	// A ref only makes sense for a git source.
	if _, err := InstallWith(context.Background(), InstallOptions{Source: src, Ref: "v1"}); err == nil {
		t.Error("ref on local source accepted")
	}
}
