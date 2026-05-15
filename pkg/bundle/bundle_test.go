package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetect_IterFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.iter")
	if err := os.WriteFile(path, []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	kind, err := Detect(path)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if kind != KindIter {
		t.Errorf("kind = %v, want KindIter", kind)
	}
}

func TestDetect_BotzArchive(t *testing.T) {
	path := fixtureMinimalBundle(t)
	kind, err := Detect(path)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if kind != KindBundle {
		t.Errorf("kind = %v, want KindBundle", kind)
	}
}

func TestDetect_BundleDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bot.iter"), []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	kind, err := Detect(dir)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if kind != KindBundleDir {
		t.Errorf("kind = %v, want KindBundleDir", kind)
	}
}

func TestDetect_DirWithoutBotIter(t *testing.T) {
	dir := t.TempDir()
	if _, err := Detect(dir); err == nil {
		t.Fatal("expected error for empty directory, got nil")
	}
}

func TestDetect_GzipMagicFallback(t *testing.T) {
	// File without recognised extension but gzip header → treat as bundle.
	src := fixtureMinimalBundle(t)
	dst := filepath.Join(t.TempDir(), "noext")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	kind, err := Detect(dst)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if kind != KindBundle {
		t.Errorf("kind = %v, want KindBundle", kind)
	}
}

func TestOpen_MinimalBundle(t *testing.T) {
	path := fixtureMinimalBundle(t)
	cacheRoot := t.TempDir()
	b, cleanup, err := Open(path, cacheRoot)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cleanup()
	if b.IterPath == "" {
		t.Fatal("IterPath empty")
	}
	if _, err := os.Stat(b.IterPath); err != nil {
		t.Errorf("IterPath not on disk: %v", err)
	}
	if b.Hash == "" {
		t.Errorf("Hash empty")
	}
	if b.SourcePath == "" {
		t.Errorf("SourcePath empty")
	}
}

func TestOpen_BundleWithSkillsPrompts(t *testing.T) {
	path := fixtureBundleWithSkillsPrompts(t)
	cacheRoot := t.TempDir()
	b, cleanup, err := Open(path, cacheRoot)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cleanup()
	if b.SkillsDir == "" {
		t.Errorf("SkillsDir not populated")
	}
	if b.PromptsDir == "" {
		t.Errorf("PromptsDir not populated")
	}
	if b.Manifest == nil {
		t.Fatal("Manifest nil")
	}
	if b.Manifest.Name != "test-bundle" {
		t.Errorf("Manifest.Name = %q", b.Manifest.Name)
	}
	// Skill file must exist on disk.
	skill := filepath.Join(b.SkillsDir, "probe.md")
	if _, err := os.Stat(skill); err != nil {
		t.Errorf("skill file missing: %v", err)
	}
}

func TestOpen_CacheHit(t *testing.T) {
	// Two consecutive Opens of the same archive should produce the same
	// cache slot — the second call is essentially a no-op extract.
	path := fixtureMinimalBundle(t)
	cacheRoot := t.TempDir()
	b1, c1, err := Open(path, cacheRoot)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer c1()
	b2, c2, err := Open(path, cacheRoot)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer c2()
	if b1.Hash != b2.Hash {
		t.Errorf("hashes differ across calls: %q vs %q", b1.Hash, b2.Hash)
	}
	if b1.Dir != b2.Dir {
		t.Errorf("cache slots differ: %q vs %q", b1.Dir, b2.Dir)
	}
}

func TestOpen_RejectsPathTraversal(t *testing.T) {
	path := fixturePathTraversal(t)
	_, _, err := Open(path, t.TempDir())
	errContains(t, err, "path traversal")
}

func TestOpen_RejectsAbsolutePath(t *testing.T) {
	path := fixtureAbsolutePath(t)
	_, _, err := Open(path, t.TempDir())
	errContains(t, err, "absolute path")
}

func TestOpen_RejectsSymlinks(t *testing.T) {
	path := fixtureSymlinkEscape(t)
	_, _, err := Open(path, t.TempDir())
	errContains(t, err, "unsupported entry type")
}

func TestOpen_EnforcesMaxBytes(t *testing.T) {
	t.Setenv("ITERION_BUNDLE_MAX_BYTES", "100")
	path := fixtureOversize(t)
	_, _, err := Open(path, t.TempDir())
	errContains(t, err, "size exceeds limit")
}

func TestOpen_RejectsBundleWithoutBotIter(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "nobot.botz")
	buildBotz(t, dest, []tarEntry{
		{Name: "manifest.yaml", Body: []byte("name: ghost\nschema_version: 1\n")},
	})
	_, _, err := Open(dest, t.TempDir())
	errContains(t, err, "no bot.iter")
}

func TestOpenDir_DiscoversResources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bot.iter"), []byte(minimalBotIter), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: dev\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := OpenDir(dir)
	if err != nil {
		t.Fatalf("opendir: %v", err)
	}
	if b.SkillsDir == "" {
		t.Errorf("SkillsDir empty")
	}
	if b.Manifest == nil || b.Manifest.Name != "dev" {
		t.Errorf("manifest not loaded: %+v", b.Manifest)
	}
	if b.Hash != "" {
		t.Errorf("Hash should be empty for KindBundleDir, got %q", b.Hash)
	}
}

func TestLoadManifest_RejectsUnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte("name: future\nschema_version: 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifest(path)
	errContains(t, err, "schema_version 99 not supported")
}

func TestLoadManifest_MissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadManifest(filepath.Join(dir, "absent.yaml"))
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if m != nil {
		t.Errorf("expected nil manifest, got %+v", m)
	}
}
