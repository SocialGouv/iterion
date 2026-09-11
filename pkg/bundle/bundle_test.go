package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
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
		// The loader reads either spelling; a marker it reads but the
		// promotion ignored gave the file and directory forms two verdicts.
		{"manifest .yml marker", "b/main.bot", []string{ManifestFileAlt}, true},
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
				// An iterion manifest: it carries the key every one of
				// ours has (a foreign one marks nothing, see below).
				if err := os.WriteFile(mp, []byte("schema_version: 1\nname: x\n"), 0o644); err != nil {
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
		if m != DirSkills && m != ManifestFile && m != ManifestFileAlt {
			t.Errorf("marker %q is none of DirSkills, ManifestFile, ManifestFileAlt", m)
		}
	}
}

// TestManifestKeysAreDerivedFromTheStruct: the key sets the marker reads
// come from the Manifest struct itself, and the generic list is exactly
// the documented one — a widened generic list would silently stop marking
// bundles, a stale entry would name a key the struct no longer has.
func TestManifestKeysAreDerivedFromTheStruct(t *testing.T) {
	wantGeneric := []string{"author", "description", "enabled", "icon", "name", "repo", "triggers", "version"}
	var gotGeneric []string
	for k := range genericManifestKeys {
		gotGeneric = append(gotGeneric, k)
		if !manifestStructKeys[k] {
			t.Errorf("generic key %q is not a Manifest key", k)
		}
	}
	sort.Strings(gotGeneric)
	if !reflect.DeepEqual(gotGeneric, wantGeneric) {
		t.Fatalf("genericManifestKeys = %v, want exactly %v", gotGeneric, wantGeneric)
	}
	for _, k := range []string{"schema_version", "display_name", "when_to_use", "invocations", "dispatch_vars", "produces", "consumes", "capabilities", "requires"} {
		if !manifestStructKeys[k] || !iterionOnlyKeys[k] {
			t.Errorf("%q is not read as a key only ours have (struct=%v, only=%v)", k, manifestStructKeys[k], iterionOnlyKeys[k])
		}
	}
	for k := range manifestStructKeys {
		if genericManifestKeys[k] == iterionOnlyKeys[k] {
			t.Errorf("%q is both generic and iterion-only, or neither", k)
		}
	}
	if len(manifestStructKeys) < 20 {
		t.Fatalf("only %d Manifest keys derived; the reflection no longer reads the struct", len(manifestStructKeys))
	}
}

// TestForeignManifestBeside: the file named like a manifest that did NOT
// mark is named, with the reason — the one outcome that would otherwise be
// silent (a typo in the only distinctive key drops the bundle's prompts,
// presets and skills with no diagnostic). Nothing is reported when there
// is no such file, or when the sibling marks.
func TestForeignManifestBeside(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string // a substring of the reason, or "" for no report
	}{
		{"typo in the only distinctive key", "name: my-bot\ndisplay_nmae: Beep\n", "display_nmae"},
		{"templated foreign", "name: {{ .Chart.Name }}\n{{- range .Values.x }}\n", "is not YAML"},
		{"marks", "name: my-bot\ndisplay_name: Beep\n", ""},
		{"generic only marks", "name: my-bot\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "b")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			mainBot := filepath.Join(dir, MainBotFile)
			if err := os.WriteFile(mainBot, []byte("workflow x:\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			p, why := ForeignManifestBeside(mainBot)
			if tc.want == "" {
				if p != "" || why != "" {
					t.Fatalf("ForeignManifestBeside = (%q, %q), want nothing reported", p, why)
				}
				return
			}
			if p != filepath.Join(dir, ManifestFile) || !strings.Contains(why, tc.want) {
				t.Fatalf("ForeignManifestBeside = (%q, %q), want the manifest named with a reason containing %q", p, why, tc.want)
			}
		})
	}
	loose := filepath.Join(t.TempDir(), MainBotFile)
	if err := os.WriteFile(loose, []byte("workflow x:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, why := ForeignManifestBeside(loose); p != "" || why != "" {
		t.Fatalf("a loose main.bot reported (%q, %q)", p, why)
	}
	if p, why := ForeignManifestBeside(filepath.Join(t.TempDir(), "other.bot")); p != "" || why != "" {
		t.Fatalf("a file that is not main.bot reported (%q, %q)", p, why)
	}
}

// TestDirForMainBot_ManifestMustBeIterions: a manifest marks a bundle only
// when it claims to be iterion's. A `manifest.yaml` of another tool beside
// a loose main.bot — a common filename — marks nothing, so the file
// compiles alone as it always did; an iterion manifest that does not
// DECODE still marks its bundle (the loader's refusal is then loud, on
// every surface, instead of a run starting without its prompts and
// skills); and a `skills/` marks regardless.
func TestDirForMainBot_ManifestMustBeIterions(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		content  string
		want     bool
	}{
		{"foreign manifest.yaml", ManifestFile, "apiVersion: v2\nname: my-chart\nversion: 1.0.0\n", false},
		{"foreign manifest.yml", ManifestFileAlt, "manifest_version: 3\nname: ext\n", false},
		// `triggers:` is ours but every CI's too: beside a foreign key it
		// says nothing, and the file marks nothing.
		{"foreign manifest with triggers", ManifestFile, "triggers:\n  - push\njobs:\n  build: {}\n", false},
		{"iterion manifest.yml", ManifestFileAlt, "schema_version: 1\nname: x\n", true},
		// A valid manifest may carry only generic keys: the loader defaults
		// schema_version, so its shape is ours and it marks.
		{"valid manifest with generic keys only", ManifestFile, "name: x\nversion: 0.1.0\ndescription: d\n", true},
		// So does a BROKEN one whose only defect is a value in a generic
		// key: still ours in shape, and the open must fail by name rather
		// than the run compiling around it.
		{"broken iterion manifest, generic keys only", ManifestFile, "name: b\nicon: \"an icon far longer than the thirty-two bytes a short emoji takes\"\n", true},
		{"broken iterion manifest, bad enabled", ManifestFile, "name: b\nenabled: yes-please\n", true},
		{"broken iterion manifest", ManifestFile, "schema_version: 99\nname: [broken\n", true},
		{"iterion key deeper than schema_version", ManifestFile, "name: x\ndisplay_name: X\n", true},
		{"broken manifest with an iterion key only", ManifestFile, "invocations: [\nname: x\n", true},
		// A foreign file whose EVERY key is one of ours is
		// indistinguishable from a broken manifest of ours: it marks, and
		// the loader refuses it by name, with the remedy in the message.
		{"foreign file made only of our keys", ManifestFile, "requires:\n  - some-tool\nname: thing\n", true},
		// Ours with a foreign key beside a distinctive one: a manifest of
		// ours with a key this build does not know yet, or a typo beside a
		// surviving distinctive key — marks, so the loader names the key.
		{"ours with an unknown key", ManifestFile, "display_name: X\nnotes: later\n", true},
		// The text fallback for a file the parser cannot read matches only
		// the DISTINCTIVE keys: a Helm-templated or tab-broken manifest of
		// another tool carries `name:` at line start too.
		{"unparsable foreign, templated", ManifestFile, "apiVersion: v1\nname: {{ .Chart.Name }}\n{{- range .Values.x }}\n", false},
		{"unparsable foreign, a tab", ManifestFile, "name: CI\njobs:\n\tbuild: {}\n", false},
		{"unparsable ours, a distinctive key survives", ManifestFile, "display_name: X\ninvocations: [\n", true},
		{"unparsable ours, distinctive key indented", ManifestFile, "x:\n  display_name: X\ninvocations: [\n  invocations: [\n", true},
		{"unparsable, distinctive key only in a comment", ManifestFile, "# invocations:\nname: [broken\n", false},
		{"empty manifest", ManifestFile, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "b")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, MainBotFile), []byte("workflow x:\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.manifest), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got := DirForMainBot(filepath.Join(dir, MainBotFile))
			if tc.want && got != dir {
				t.Errorf("DirForMainBot = %q, want %q", got, dir)
			}
			if !tc.want && got != "" {
				t.Errorf("DirForMainBot = %q, want \"\" (a manifest that is not iterion's marks nothing)", got)
			}
		})
	}
	// A fifo named like a manifest is not a manifest — and must not block:
	// os.Open on a fifo waits for a writer, and this runs inside HTTP
	// handlers with no timeout of their own.
	if runtime.GOOS != "windows" {
		fdir := filepath.Join(t.TempDir(), "b")
		if err := os.MkdirAll(fdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fdir, MainBotFile), []byte("workflow x:\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(fdir, ManifestFile), 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		done := make(chan string, 1)
		go func() { done <- DirForMainBot(filepath.Join(fdir, MainBotFile)) }()
		select {
		case got := <-done:
			if got != "" {
				t.Errorf("a fifo named manifest.yaml marked a bundle: %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("DirForMainBot blocked on a fifo named manifest.yaml (os.Open before Stat)")
		}
	}
	// A file past the read bound is not ours: judging the half a bound
	// left readable would be judging another document.
	big := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(big, MainBotFile), []byte("workflow x:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	huge := []byte("display_name: X\ndata: |\n")
	for len(huge) <= maxManifestProbe {
		huge = append(huge, "  0123456789012345678901234567890123456789012345678901234567890123456789\n"...)
	}
	if err := os.WriteFile(filepath.Join(big, ManifestFile), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DirForMainBot(filepath.Join(big, MainBotFile)); got != "" {
		t.Errorf("a manifest past the read bound marked a bundle: %q", got)
	}
	// A directory named like a manifest is not a manifest.
	dir := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(filepath.Join(dir, ManifestFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, MainBotFile), []byte("workflow x:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DirForMainBot(filepath.Join(dir, MainBotFile)); got != "" {
		t.Errorf("a directory named manifest.yaml marked a bundle: %q", got)
	}
}
