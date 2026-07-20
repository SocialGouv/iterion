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
