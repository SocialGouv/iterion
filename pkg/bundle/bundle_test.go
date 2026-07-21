package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetect_BotFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.bot")
	if err := os.WriteFile(path, []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	kind, err := Detect(path)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if kind != KindBot {
		t.Errorf("kind = %v, want KindBot", kind)
	}
}

func TestDetect_RejectsUnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Detect(path)
	errContains(t, err, "expected .bot or .botz")
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
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("# stub"), 0o644); err != nil {
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

func TestDetect_DirWithoutBot(t *testing.T) {
	dir := t.TempDir()
	if _, err := Detect(dir); err == nil {
		t.Fatal("expected error for empty directory, got nil")
	}
}

func TestDetect_RejectsBundleWithoutBotzExtension(t *testing.T) {
	src := fixtureMinimalBundle(t)
	dst := filepath.Join(t.TempDir(), "noext")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Detect(dst)
	errContains(t, err, "expected .bot or .botz")
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
	errContains(t, err, "no main.bot")
}

func TestOpenDir_DiscoversResources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(minimalBotIter), 0o644); err != nil {
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

func TestLoadManifest_RejectsAttachmentTraversal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	body := "name: evil\nschema_version: 1\nattachments:\n  secret: ../../../../etc/passwd\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifest(path)
	errContains(t, err, "escapes the bundle")
}

func TestLoadManifest_RejectsAbsoluteAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	body := "name: evil\nschema_version: 1\nattachments:\n  secret: /etc/passwd\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifest(path)
	errContains(t, err, "absolute")
}

func TestLoadManifest_AllowsRelativeAttachment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	// A nested path with an internal ".." that cancels out must still be
	// accepted — it does not escape the bundle.
	body := "name: ok\nschema_version: 1\nattachments:\n  logo: images/logo.png\n  doc: a/../b/readme.md\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil || m.Attachments["logo"] != "images/logo.png" {
		t.Fatalf("manifest not loaded correctly: %+v", m)
	}
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

func TestLoadManifest_ParsesAndNormalizesLaunchHints(t *testing.T) {
	body := `name: appy
schema_version: 1
launch:
  primary: ["  app_prompt ", "mode", "", "app_prompt", "budget"]
  hidden: [" internal_var", "internal_var", "   "]
`
	m, err := LoadManifest(writeManifestForTest(t, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Launch == nil {
		t.Fatal("launch block not parsed")
	}
	// Trimmed, empties dropped, deduped keeping first-occurrence order.
	wantPrimary := []string{"app_prompt", "mode", "budget"}
	if len(m.Launch.Primary) != len(wantPrimary) {
		t.Fatalf("primary = %v, want %v", m.Launch.Primary, wantPrimary)
	}
	for i, w := range wantPrimary {
		if m.Launch.Primary[i] != w {
			t.Errorf("primary[%d] = %q, want %q (order must be preserved)", i, m.Launch.Primary[i], w)
		}
	}
	if len(m.Launch.Hidden) != 1 || m.Launch.Hidden[0] != "internal_var" {
		t.Errorf("hidden = %v, want [internal_var]", m.Launch.Hidden)
	}
}

func TestLoadManifest_LaunchHints_EmptyCollapsesToNil(t *testing.T) {
	// A block whose lists are all-blank normalizes away entirely so the
	// bot entry's JSON omits `launch` (omitempty on a nil pointer).
	body := "name: appy\nschema_version: 1\nlaunch:\n  primary: [\"\", \"  \"]\n"
	m, err := LoadManifest(writeManifestForTest(t, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Launch != nil {
		t.Errorf("expected nil Launch after normalization, got %+v", m.Launch)
	}

	m, err = LoadManifest(writeManifestForTest(t, "name: plain\nschema_version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Launch != nil {
		t.Errorf("expected nil Launch when block absent, got %+v", m.Launch)
	}
}

// TestDirForMainBot is the shared answer to "is this main.bot inside a
// bundle?" — pkg/cli and pkg/runview both route through it, so a
// disagreement between them is no longer possible.
func TestDirForMainBot(t *testing.T) {
	tests := []struct {
		name    string
		file    string // path (relative to a temp root) to create as main.bot
		markers []string
		want    bool
	}{
		{"skills marker", "b/main.bot", []string{DirSkills}, true},
		{"manifest marker", "b/main.bot", []string{ManifestFile}, true},
		{"both markers", "b/main.bot", []string{DirSkills, ManifestFile}, true},
		{"no marker", "b/main.bot", nil, false},
		{"not main.bot", "b/other.bot", []string{DirSkills, ManifestFile}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tt.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("workflow x:\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, m := range tt.markers {
				mp := filepath.Join(filepath.Dir(path), m)
				if m == DirSkills {
					if err := os.MkdirAll(mp, 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(mp, []byte("name: x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got := DirForMainBot(path)
			if tt.want && got != filepath.Dir(path) {
				t.Errorf("DirForMainBot = %q, want %q", got, filepath.Dir(path))
			}
			if !tt.want && got != "" {
				t.Errorf("DirForMainBot = %q, want \"\"", got)
			}
		})
	}
}

// TestDirForMainBot_MarkersAreLayoutConstants keeps the marker list tied
// to the exported layout names: a rename that updated only one of them
// would otherwise leave bundle detection silently looking for a
// directory that no longer exists.
func TestDirForMainBot_MarkersAreLayoutConstants(t *testing.T) {
	for _, m := range dirMarkers {
		if m != DirSkills && m != ManifestFile {
			t.Errorf("marker %q is neither DirSkills nor ManifestFile", m)
		}
	}
}
