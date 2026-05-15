package bundle

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// buildSampleSource lays out a minimal bundle source tree under a tempdir
// and returns the path. Used by writer tests to exercise the happy path
// without dragging in DSL fixtures.
func buildSampleSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"bot.iter":          minimalBotIter,
		"manifest.yaml":     "name: writer-test\nversion: 0.1.0\nschema_version: 1\n",
		"skills/probe.md":   "# probe\n",
		"prompts/helper.md": "Helper body.\n",
		"README.md":         "# writer-test\n",
	}
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPackDir_RoundTripWithLoader(t *testing.T) {
	src := buildSampleSource(t)
	out := filepath.Join(t.TempDir(), "out.botz")

	res, err := PackDir(src, out)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if res.Entries < 5 {
		t.Errorf("entries = %d, want >= 5", res.Entries)
	}
	if res.Hash == "" {
		t.Errorf("hash empty")
	}

	// Open via the consumer loader — verifies the archive is well-formed
	// AND that the writer's hash matches the loader's hash byte-for-byte.
	cacheRoot := t.TempDir()
	b, cleanup, err := Open(out, cacheRoot)
	if err != nil {
		t.Fatalf("open packed bundle: %v", err)
	}
	defer cleanup()
	if b.Hash != res.Hash {
		t.Errorf("hash drift: writer=%s loader=%s", res.Hash, b.Hash)
	}
	if b.SkillsDir == "" {
		t.Errorf("SkillsDir empty after round-trip")
	}
	if b.PromptsDir == "" {
		t.Errorf("PromptsDir empty after round-trip")
	}
	if b.Manifest == nil || b.Manifest.Name != "writer-test" {
		t.Errorf("manifest not preserved: %+v", b.Manifest)
	}
}

func TestPackDir_Deterministic(t *testing.T) {
	src := buildSampleSource(t)
	dir := t.TempDir()

	a, err := PackDir(src, filepath.Join(dir, "a.botz"))
	if err != nil {
		t.Fatalf("first pack: %v", err)
	}
	b, err := PackDir(src, filepath.Join(dir, "b.botz"))
	if err != nil {
		t.Fatalf("second pack: %v", err)
	}
	if a.Hash != b.Hash {
		t.Errorf("hashes differ across packs: %q vs %q", a.Hash, b.Hash)
	}
	// Stronger check: compressed bytes are also bit-identical.
	aBytes, err := os.ReadFile(a.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	bBytes, err := os.ReadFile(b.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(aBytes, bBytes) {
		t.Errorf("compressed output differs: %d vs %d bytes", len(aBytes), len(bBytes))
	}
}

func TestPackDir_RefusesMissingBotIter(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "manifest.yaml"), []byte("name: x\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "no-bot.botz")
	_, err := PackDir(src, out)
	errContains(t, err, "no bot.iter")
}

func TestPackDir_RefusesSymlinks(t *testing.T) {
	src := buildSampleSource(t)
	// Drop a symlink inside the source tree.
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "evil-link")); err != nil {
		t.Skipf("symlink not supported on this platform: %v", err)
	}
	out := filepath.Join(t.TempDir(), "with-symlink.botz")
	_, err := PackDir(src, out)
	errContains(t, err, "symlink")
}

func TestPackDir_RefusesExistingOutput(t *testing.T) {
	src := buildSampleSource(t)
	out := filepath.Join(t.TempDir(), "out.botz")
	if err := os.WriteFile(out, []byte("collision"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := PackDir(src, out)
	errContains(t, err, "already exists")
}

func TestPackDir_SkipsBotzAndIterionStore(t *testing.T) {
	src := buildSampleSource(t)
	// Drop files that must be filtered out before packing.
	if err := os.WriteFile(filepath.Join(src, "old-build.botz"), []byte("noise"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".iterion", "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".iterion", "runs", "stale.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "clean.botz")
	res, err := PackDir(src, out)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	// Extract via the loader and check the noise is absent.
	cacheRoot := t.TempDir()
	b, cleanup, err := Open(out, cacheRoot)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(b.Dir, "old-build.botz")); err == nil {
		t.Errorf("old-build.botz leaked into the archive")
	}
	if _, err := os.Stat(filepath.Join(b.Dir, ".iterion")); err == nil {
		t.Errorf(".iterion/ leaked into the archive")
	}
	_ = res
}
