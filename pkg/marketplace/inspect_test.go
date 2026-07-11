package marketplace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

// writePluginFixture writes a minimal valid plugin source: a plugin.yaml
// (name + a skills contribution — the smallest shape plugin.Validate
// accepts) and the contributed skill file.
func writePluginFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	man := strings.Join([]string{
		"name: my-plugin",
		"version: 0.2.0",
		"description: a test plugin",
		"author: jo",
		"contributes:",
		"  skills:",
		"    - skills/foo.md",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: foo\ndescription: test skill\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "skills", "foo.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeBotFixture writes a minimal valid bot bundle (main.bot +
// manifest.yaml — what botinstall.Inspect needs).
func writeBotFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("# mybot\nworkflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	man := "name: mybot\nversion: 0.1.0\ndescription: test bot\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectSource_Plugin(t *testing.T) {
	dir := t.TempDir()
	writePluginFixture(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# plugin doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	si, err := InspectSource(context.Background(), dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if si.Kind != KindPlugin {
		t.Errorf("Kind = %q, want %q", si.Kind, KindPlugin)
	}
	if si.Plugin == nil || si.Plugin.Manifest == nil {
		t.Fatal("Plugin payload missing")
	}
	if si.Bot != nil {
		t.Errorf("Bot payload should be nil for a plugin source")
	}
	if si.Plugin.Manifest.Name != "my-plugin" {
		t.Errorf("Manifest.Name = %q", si.Plugin.Manifest.Name)
	}
	if !strings.Contains(si.Plugin.README, "plugin doc") {
		t.Errorf("README = %q", si.Plugin.README)
	}
}

func TestInspectSource_Bot(t *testing.T) {
	dir := t.TempDir()
	writeBotFixture(t, dir)
	si, err := InspectSource(context.Background(), dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if si.Kind != KindBot {
		t.Errorf("Kind = %q, want %q", si.Kind, KindBot)
	}
	if si.Bot == nil {
		t.Fatal("Bot payload missing")
	}
	if si.Plugin != nil {
		t.Errorf("Plugin payload should be nil for a bot source")
	}
	if si.Bot.Name != "mybot" {
		t.Errorf("Bot.Name = %q", si.Bot.Name)
	}
}

func TestInspectSource_PathSubdir(t *testing.T) {
	root := t.TempDir()
	writePluginFixture(t, filepath.Join(root, "sub", "p"))
	si, err := InspectSource(context.Background(), root, "", filepath.Join("sub", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if si.Kind != KindPlugin {
		t.Errorf("Kind = %q, want %q", si.Kind, KindPlugin)
	}
}

func TestInspectSource_PathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	writePluginFixture(t, root)
	if _, err := InspectSource(context.Background(), root, "", "../outside"); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
}

func TestInspectSource_NeitherReportsBothCauses(t *testing.T) {
	dir := t.TempDir() // empty: no plugin.yaml, no bundle
	_, err := InspectSource(context.Background(), dir, "", "")
	if err == nil {
		t.Fatal("expected error for an empty source dir")
	}
	msg := err.Error()
	if !strings.Contains(msg, "not a plugin (no plugin.yaml)") {
		t.Errorf("error missing plugin cause: %q", msg)
	}
	if !strings.Contains(msg, "not a bot bundle") {
		t.Errorf("error missing bot cause: %q", msg)
	}
}

func TestInspectSource_MalformedPluginManifestPropagates(t *testing.T) {
	dir := t.TempDir()
	// plugin.yaml exists but contributes nothing → plugin.Validate error,
	// which must propagate as-is (no silent fallback to the bot probe).
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := InspectSource(context.Background(), dir, "", "")
	if err == nil {
		t.Fatal("expected malformed plugin.yaml to error")
	}
	if strings.Contains(err.Error(), "not a bot bundle") {
		t.Errorf("malformed plugin.yaml fell through to the bot probe: %q", err)
	}
	if !strings.Contains(err.Error(), "contributes nothing") {
		t.Errorf("expected the plugin validation error, got %q", err)
	}
}

func TestEntryFromPlugin(t *testing.T) {
	info := &plugin.InspectInfo{
		Manifest: &plugin.Manifest{
			Name:        "My_Plugin",
			Version:     "1.0.0",
			Description: "does things",
			Author:      "jo",
			Contributes: plugin.Contributes{Skills: []string{"skills/foo.md"}},
		},
		README: "# doc",
	}
	e := EntryFromPlugin(info, "https://example.com/repo.git", "dev", "sub/p")
	if e.Slug != "my-plugin" {
		t.Errorf("Slug = %q, want %q (NormalizeName)", e.Slug, "my-plugin")
	}
	if e.Kind != KindPlugin {
		t.Errorf("Kind = %q", e.Kind)
	}
	if len(e.Categories) != 1 || e.Categories[0] != "skill" {
		t.Errorf("Categories = %v, want [skill]", e.Categories)
	}
	if e.Name != "My_Plugin" || e.Version != "1.0.0" || e.Description != "does things" || e.Author != "jo" {
		t.Errorf("manifest fields not carried over: %+v", e)
	}
	if e.README != "# doc" {
		t.Errorf("README = %q", e.README)
	}
	if e.RepoURL != "https://example.com/repo.git" || e.Ref != "dev" || e.Subpath != "sub/p" {
		t.Errorf("install coordinates not carried over: %+v", e)
	}
	if e.Source != SourceGit {
		t.Errorf("Source = %q", e.Source)
	}
}

func TestSplitSourceRef(t *testing.T) {
	url, ref := splitSourceRef("https://example.com/repo.git#dev")
	if url != "https://example.com/repo.git" || ref != "dev" {
		t.Errorf("split = (%q, %q)", url, ref)
	}
	// A '#' whose prefix exists on disk is part of the path, not a ref
	// marker (botinstall parity: local dirs take no ref).
	root := t.TempDir()
	local := filepath.Join(root, "repo")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	url, ref = splitSourceRef(local + "#dev")
	if url != local+"#dev" || ref != "" {
		t.Errorf("split = (%q, %q), want (%q, %q)", url, ref, local+"#dev", "")
	}
	url, ref = splitSourceRef("plain-source")
	if url != "plain-source" || ref != "" {
		t.Errorf("split = (%q, %q)", url, ref)
	}
}
