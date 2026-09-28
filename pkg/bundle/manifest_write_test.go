package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

const manifestWithComments = `# Top-of-file note about this bot.
name: testbot
display_name: Testy
# the catalogue blurb
description: |
  A test bot.
  Second line.
author: me <me@example.com>
schema_version: 1
triggers: [refactor, review]
`

func TestWriteManifest_PreservesCommentsAndBlockScalar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := WriteManifest(path, ManifestPatch{DisplayName: ptr("Renamed")})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.DisplayName != "Renamed" {
		t.Errorf("DisplayName = %q, want Renamed", m.DisplayName)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"# Top-of-file note about this bot.",
		"# the catalogue blurb",
		"description: |",
		"Second line.",
		"display_name: Renamed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten manifest missing %q\n---\n%s", want, got)
		}
	}
	// The edited value must be gone.
	if strings.Contains(got, "Testy") {
		t.Errorf("old display_name still present\n---\n%s", got)
	}
}

func TestWriteManifest_PreservesLaunchHints(t *testing.T) {
	// The studio metadata PUT never patches launch:, so an operator saving
	// display_name must not strip the block — the node-level rewrite keeps
	// untouched keys, and the strict pre-write validation must accept the
	// field (it would reject an unknown key).
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	src := `name: appy
schema_version: 1
description: Builds an app.
launch:
  primary: [app_prompt, mode]
  hidden: [internal_var]
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := WriteManifest(path, ManifestPatch{DisplayName: ptr("Appy")})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.DisplayName != "Appy" {
		t.Errorf("DisplayName = %q, want Appy", m.DisplayName)
	}
	if m.Launch == nil || len(m.Launch.Primary) != 2 || m.Launch.Primary[0] != "app_prompt" ||
		m.Launch.Primary[1] != "mode" || len(m.Launch.Hidden) != 1 || m.Launch.Hidden[0] != "internal_var" {
		t.Errorf("launch hints not preserved by re-parse: %+v", m.Launch)
	}

	raw, _ := os.ReadFile(path)
	got := string(raw)
	for _, want := range []string{"launch:", "primary:", "hidden:", "app_prompt", "internal_var"} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten manifest lost %q\n---\n%s", want, got)
		}
	}
}

func TestWriteManifest_AppendsNewKeysAfterDescription(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteManifest(path, ManifestPatch{
		WhenToUse: ptr("Use when testing.\nSecond hint."),
		Enabled:   ptr(false),
	}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)

	if !strings.Contains(got, "when_to_use: |") {
		t.Errorf("multi-line when_to_use should use block-literal style\n---\n%s", got)
	}
	if !strings.Contains(got, "enabled: false") {
		t.Errorf("enabled should be an unquoted bool\n---\n%s", got)
	}
	// New keys land between description and author.
	descAt := strings.Index(got, "description:")
	whenAt := strings.Index(got, "when_to_use:")
	authorAt := strings.Index(got, "author:")
	if descAt >= whenAt || whenAt >= authorAt {
		t.Errorf("when_to_use not placed after description / before author (desc=%d when=%d author=%d)\n---\n%s",
			descAt, whenAt, authorAt, got)
	}

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m.WhenToUse == "" || m.IsEnabled() {
		t.Errorf("reload: WhenToUse=%q IsEnabled=%v, want set + disabled", m.WhenToUse, m.IsEnabled())
	}
	// The anchor computation must see the block scalar's WHOLE span: the
	// inserted keys belong below it, and the anchored value is intact.
	// (Round 1: the insert landed between `description: |` and its body,
	// the body decoded into when_to_use, and description came back empty —
	// a placement-only assertion never saw it.)
	if m.Description != "A test bot.\nSecond line.\n" {
		t.Errorf("reload: Description=%q, want the authored block intact", m.Description)
	}
	if m.WhenToUse != "Use when testing.\nSecond hint." {
		t.Errorf("reload: WhenToUse=%q, want exactly the patched value", m.WhenToUse)
	}
}

// Patching a block-valued key replaces its WHOLE span: the old body does
// not survive the splice (round 1: it did, and decoded into the new value).
func TestWriteManifest_PatchesABlockValuedKeyWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := WriteManifest(path, ManifestPatch{Description: ptr("A new blurb.\nTwo lines.\n")})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.Description != "A new blurb.\nTwo lines.\n" {
		t.Fatalf("Description=%q — the old body was absorbed", m.Description)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if strings.Contains(got, "A test bot.") {
		t.Errorf("the old body is still on disk\n---\n%s", got)
	}
	if !strings.Contains(got, "# the catalogue blurb") {
		t.Errorf("the block's head comment was lost\n---\n%s", got)
	}
}

// `|+` keeps the value's trailing newlines — exactly them: the strings.Split
// artifact is not a content line, and a value of only newlines takes the
// double-quoted form (a contentless block reads back empty).
func TestWriteManifest_KeepChompingRoundTripsExactly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{Description: ptr("Line one.\n\n")}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "description: |+") {
		t.Fatalf("two trailing newlines want keep chomping:\n%s", raw)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m.Description != "Line one.\n\n" {
		t.Fatalf("Description=%q, want %q", m.Description, "Line one.\n\n")
	}
	// A second patch of the same value must not grow it.
	if _, err := WriteManifest(path, ManifestPatch{Description: ptr("Line one.\n\n")}); err != nil {
		t.Fatalf("WriteManifest 2: %v", err)
	}
	m, _ = LoadManifest(path)
	if m.Description != "Line one.\n\n" {
		t.Fatalf("Description grew on re-patch: %q", m.Description)
	}

	// An all-newline value has no content line for a block: double-quoted.
	if _, err := WriteManifest(path, ManifestPatch{Description: ptr("\n\n")}); err != nil {
		t.Fatalf("WriteManifest all-newline: %v", err)
	}
	m, _ = LoadManifest(path)
	if m.Description != "\n\n" {
		t.Fatalf("all-newline Description=%q", m.Description)
	}
}

// A comment on a block MAPPING's own line (`requires: # floor note`) rides
// the key node, not the value: it is coalesced onto the new head line.
// (A block scalar's `| # note` rides the value node instead — yaml.v3's
// split, probed; both are covered.)
func TestWriteManifest_BlockKeyLineCommentSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	src := "name: testbot\nschema_version: 1\nrequires: # floor note\n  iterion: \">= 3.141.0\"\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{Requires: &Requires{Iterion: ">= 3.205.2"}}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "requires: # floor note") {
		t.Errorf("the key-line comment was dropped\n---\n%s", raw)
	}
}

// A foot comment under a block mapping is not the mapping's content: a
// patch of the block leaves it in place (round 1: the span swallowed it).
func TestWriteManifest_BlockFootCommentSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	src := "name: testbot\nschema_version: 1\nrequires:\n  iterion: \">= 3.141.0\"\n  # why the floor is here\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{Requires: &Requires{Iterion: ">= 3.205.2"}}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if !strings.Contains(got, "# why the floor is here") {
		t.Errorf("the block's foot comment was deleted\n---\n%s", got)
	}
	if !strings.Contains(got, `iterion: ">= 3.205.2"`) {
		t.Errorf("the floor was not raised\n---\n%s", got)
	}
}

// YAML 1.1 spellings are never written plain: `yes` is a string to yaml.v3
// but a bool to the v2 loader the gate uses. The probe is the v2 decoder
// itself — not a denylist — so the rule cannot drift from the reader, and
// it covers the whole class (hex, octal, exponents, .inf) rather than the
// two spellings someone listed. `1:20` reads as a string in BOTH readers
// (the v2 fork has no sexagesimal), so it may stay plain.
func TestWriteManifest_Yaml11WordsStayQuoted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte("name: testbot\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"yes", "Off", "n", "0x10", "010"} {
		if _, err := WriteManifest(path, ManifestPatch{DisplayName: ptr(v)}); err != nil {
			t.Fatalf("WriteManifest(%q): %v", v, err)
		}
		m, err := LoadManifest(path)
		if err != nil {
			t.Fatalf("reload(%q): %v", v, err)
		}
		if m.DisplayName != v {
			t.Errorf("DisplayName=%q, want %q", m.DisplayName, v)
		}
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), "display_name: "+v+"\n") {
			t.Errorf("%q was written plain\n---\n%s", v, raw)
		}
	}
	// The probe must not over-quote: a shape both readers read as the
	// string stays plain.
	if _, err := WriteManifest(path, ManifestPatch{DisplayName: ptr("1:20")}); err != nil {
		t.Fatalf("WriteManifest(1:20): %v", err)
	}
	m, _ := LoadManifest(path)
	if m.DisplayName != "1:20" {
		t.Errorf("DisplayName=%q, want %q", m.DisplayName, "1:20")
	}
}

// A quoted scalar spanning lines owes its continuation no indentation, and
// a flow collection's brackets balance where they balance: both are
// replaced over their WHOLE span, not to their first line.
func TestWriteManifest_SpanningValuesPatchWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	src := `name: testbot
description: "A long blurb that
  wraps over a second line"
triggers: [refactor,
  review]
author: me
schema_version: 1
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := WriteManifest(path, ManifestPatch{
		Description: ptr("Short."),
		Triggers:    ptr([]string{"triage"}),
	})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.Description != "Short." {
		t.Errorf("Description=%q — the quoted continuation survived the splice", m.Description)
	}
	if len(m.Triggers) != 1 || m.Triggers[0] != "triage" {
		t.Errorf("Triggers=%v — the flow tail survived the splice", m.Triggers)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	if strings.Contains(got, "wraps over") || strings.Contains(got, "  review") {
		t.Errorf("the old span is still on disk\n---\n%s", got)
	}
}

// A manifest holding anchors or aliases cannot be text-patched (the tree
// parse resolves them, so a span is not the value's): a patch is refused by
// name and the file is untouched; a no-op patch still goes through.
func TestWriteManifest_AnchoredManifestIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	src := "name: testbot\nschema_version: 1\ndescription: &d |\n  Anchored.\nauthor: me\nwhen_to_use: *d\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{Author: ptr("someone")}); err == nil || !strings.Contains(err.Error(), "anchors or aliases") {
		t.Fatalf("an anchored manifest must refuse the patch by name: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != src {
		t.Errorf("the refused patch landed anyway\n---\n%s", raw)
	}
	if _, err := WriteManifest(path, ManifestPatch{}); err != nil {
		t.Fatalf("a no-op patch on an anchored manifest: %v", err)
	}
}

func TestWriteManifest_IconRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := WriteManifest(path, ManifestPatch{Icon: ptr("🦉")})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.Icon != "🦉" {
		t.Errorf("Icon = %q, want 🦉", m.Icon)
	}

	raw, _ := os.ReadFile(path)
	got := string(raw)
	// New key lands right after display_name; comments survive.
	displayAt := strings.Index(got, "display_name:")
	iconAt := strings.Index(got, "icon:")
	descAt := strings.Index(got, "description:")
	if displayAt >= iconAt || iconAt >= descAt {
		t.Errorf("icon not placed after display_name / before description (display=%d icon=%d desc=%d)\n---\n%s",
			displayAt, iconAt, descAt, got)
	}
	if !strings.Contains(got, "# the catalogue blurb") {
		t.Errorf("comment lost\n---\n%s", got)
	}

	// Clearing keeps the key but empties the value.
	m, err = WriteManifest(path, ManifestPatch{Icon: ptr("")})
	if err != nil {
		t.Fatalf("WriteManifest clear: %v", err)
	}
	if m.Icon != "" {
		t.Errorf("Icon after clear = %q, want empty", m.Icon)
	}
}

// #1349: a one-key patch leaves every other byte alone — the non-BMP icon
// is not escaped into "\U0001F9ED", and the blank lines between top-level
// keys do not vanish.
func TestWriteManifest_RequiresPatchKeepsEmojiAndBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	src := `# Top-of-file note about this bot.
name: compass
icon: 🧭

# the catalogue blurb
description: |
  A test bot.
  Second line.

author: me <me@example.com>
schema_version: 1

requires:
  iterion: ">= 3.141.0"
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := WriteManifest(path, ManifestPatch{Requires: &Requires{Iterion: ">= 3.204.2"}})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.Requires == nil || m.Requires.Iterion != ">= 3.204.2" {
		t.Fatalf("the floor was not raised: %+v", m.Requires)
	}

	raw, _ := os.ReadFile(path)
	got := string(raw)
	want := `# Top-of-file note about this bot.
name: compass
icon: 🧭

# the catalogue blurb
description: |
  A test bot.
  Second line.

author: me <me@example.com>
schema_version: 1

requires:
  iterion: ">= 3.204.2"
`
	if got != want {
		t.Errorf("the patch touched more than the requires: block\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, `\U0001F9ED`) {
		t.Errorf("the emoji icon came back escaped\n---\n%s", got)
	}
}

// The patched icon itself is written as the character, in place, with the
// line's comment kept.
func TestWriteManifest_PatchedIconIsWrittenRaw(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	src := "name: compass\nicon: 🧭 # the identity\nschema_version: 1\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{Icon: ptr("🦉")}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	raw, _ := os.ReadFile(path)
	want := "name: compass\nicon: 🦉 # the identity\nschema_version: 1\n"
	if string(raw) != want {
		t.Errorf("patched icon line changed shape\n--- got ---\n%s\n--- want ---\n%s", raw, want)
	}
}

func TestWriteManifest_IconTooLongRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{Icon: ptr(strings.Repeat("x", 40))}); err == nil {
		t.Fatal("expected an error for an over-long icon, got nil")
	}
	// The original file must be untouched (validation happens pre-write).
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "icon:") {
		t.Errorf("invalid icon landed on disk\n---\n%s", raw)
	}
}

func TestWriteManifest_NilPatchPreservesEverything(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m.Name != "testbot" || m.DisplayName != "Testy" || m.Author != "me <me@example.com>" {
		t.Errorf("nil patch altered values: %+v", m)
	}
	if len(m.Triggers) != 2 || m.Triggers[0] != "refactor" {
		t.Errorf("nil patch altered triggers: %v", m.Triggers)
	}
}

func TestWriteManifest_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithComments), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := ManifestPatch{WhenToUse: ptr("Use when X."), Enabled: ptr(true)}
	if _, err := WriteManifest(path, patch); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if _, err := WriteManifest(path, patch); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("re-applying the same patch changed the file\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestWriteManifest_StringLooksLikeBoolStaysString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte("name: b\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{DisplayName: ptr("true")}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if m.DisplayName != "true" {
		t.Errorf("DisplayName = %q, want the string \"true\"", m.DisplayName)
	}
}

func TestWriteManifest_CreatesScaffoldWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	m, err := WriteManifest(path, ManifestPatch{Name: ptr("fresh"), DisplayName: ptr("Freshy")})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.Name != "fresh" || m.SchemaVersion != CurrentManifestSchema {
		t.Errorf("scaffold manifest = %+v", m)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("manifest not created: %v", err)
	}
}

func TestWriteManifest_LeavesNoTempOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte("name: b\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(path, ManifestPatch{Author: ptr("x")}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
