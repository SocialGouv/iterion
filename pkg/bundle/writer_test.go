package bundle

import (
	"archive/zip"
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
		"main.bot":          minimalBotIter,
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

	// The output must be a real ZIP archive: leading PK\x03\x04 magic,
	// and archive/zip must be able to list it (the whole point — a
	// downloaded .botz now extracts with unzip / double-click).
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte{0x50, 0x4b, 0x03, 0x04}) {
		t.Fatalf("output is not a ZIP (missing PK\\x03\\x04 magic): % x", raw[:4])
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("archive/zip cannot read the .botz: %v", err)
	}
	var sawBot bool
	for _, f := range zr.File {
		if f.Name == "main.bot" {
			sawBot = true
		}
	}
	if !sawBot {
		t.Errorf("zip listing missing main.bot")
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

// TestOpen_ReadsLegacyTarGz proves backward compat: a `.botz` built with
// the OLD gzip+tar path (via buildBotz, the legacy format) still loads
// through Open after the migration to ZIP.
func TestOpen_ReadsLegacyTarGz(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "legacy.botz")
	buildBotz(t, dest, []tarEntry{
		{Name: "main.bot", Body: []byte(minimalBotIter)},
		{Name: "manifest.yaml", Body: []byte("name: legacy-bundle\nversion: 0.1.0\nschema_version: 1\n")},
		{Name: "skills/", Typeflag: 0},
		{Name: "skills/probe.md", Body: []byte("# probe\n")},
	})

	b, cleanup, err := Open(dest, t.TempDir())
	if err != nil {
		t.Fatalf("open legacy tar.gz bundle: %v", err)
	}
	defer cleanup()
	if b.IterPath == "" {
		t.Errorf("legacy bundle: main.bot not resolved")
	}
	if b.SkillsDir == "" {
		t.Errorf("legacy bundle: SkillsDir empty")
	}
	if b.Manifest == nil || b.Manifest.Name != "legacy-bundle" {
		t.Errorf("legacy bundle: manifest not preserved: %+v", b.Manifest)
	}
	if b.Hash == "" {
		t.Errorf("legacy bundle: hash empty")
	}
}

// TestContentHash_StableAcrossFormats proves the content hash is
// format-independent: the SAME files produce the SAME hash whether packed
// as a ZIP (PackDir) or read back from a legacy tar.gz (buildBotz). This
// is what keeps cache keys and persisted run hashes stable across the
// format migration.
func TestContentHash_StableAcrossFormats(t *testing.T) {
	bot := []byte(minimalBotIter)
	manifest := []byte("name: x\nversion: 0.1.0\nschema_version: 1\n")
	skill := []byte("# probe\n")

	// ZIP: pack a source dir holding exactly these files.
	src := t.TempDir()
	for rel, body := range map[string][]byte{
		"main.bot":        bot,
		"manifest.yaml":   manifest,
		"skills/probe.md": skill,
	} {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	zipOut := filepath.Join(t.TempDir(), "zip.botz")
	zipRes, err := PackDir(src, zipOut)
	if err != nil {
		t.Fatalf("pack zip: %v", err)
	}
	zipBundle, zc, err := Open(zipOut, t.TempDir())
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zc()

	// tar.gz: the SAME files via the legacy path.
	tgz := filepath.Join(t.TempDir(), "legacy.botz")
	buildBotz(t, tgz, []tarEntry{
		{Name: "main.bot", Body: bot},
		{Name: "manifest.yaml", Body: manifest},
		{Name: "skills/probe.md", Body: skill},
	})
	tgzBundle, tc, err := Open(tgz, t.TempDir())
	if err != nil {
		t.Fatalf("open tar.gz: %v", err)
	}
	defer tc()

	if zipRes.Hash != zipBundle.Hash {
		t.Errorf("writer/loader hash drift on zip: %s vs %s", zipRes.Hash, zipBundle.Hash)
	}
	if zipBundle.Hash != tgzBundle.Hash {
		t.Errorf("content hash differs across formats: zip=%s tar.gz=%s (same files must hash identically)", zipBundle.Hash, tgzBundle.Hash)
	}
}

func TestPackDir_RefusesMissingBotIter(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "manifest.yaml"), []byte("name: x\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "no-bot.botz")
	_, err := PackDir(src, out)
	errContains(t, err, "no main.bot")
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
