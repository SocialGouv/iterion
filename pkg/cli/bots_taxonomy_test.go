package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// writeTaxonomyBots writes two bundles (a verifier and an uncategorized
// one) into a temp workspace and returns the dir.
func writeTaxonomyBots(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "revi", "manifest.yaml"), `name: revi
display_name: Revi
description: Reviews diffs.
category: verify
tags: [code-review, read-only]
`)
	writeFile(t, filepath.Join(dir, "revi", "main.bot"), "agent x:\n  model: \"test\"\n")
	writeFile(t, filepath.Join(dir, "bare", "manifest.yaml"), "name: bare\ndescription: No taxonomy.\n")
	writeFile(t, filepath.Join(dir, "bare", "main.bot"), "agent x:\n  model: \"test\"\n")
	return dir
}

func listJSON(t *testing.T, opts BotsListOptions) string {
	t.Helper()
	opts.Paths = []string{opts.Paths[0]}
	opts.Format = "json"
	var buf bytes.Buffer
	if err := BotsList(opts, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestBotsList_CategoryFilterFormNormalized(t *testing.T) {
	dir := writeTaxonomyBots(t)
	// Uppercase form must match the lowercased manifest declaration.
	if out := listJSON(t, BotsListOptions{Paths: []string{dir}, Categories: []string{"VERIFY"}}); !strings.Contains(out, `"revi"`) || strings.Contains(out, `"bare"`) {
		t.Errorf("category filter lost revi or kept bare:\n%s", out)
	}
	// The pseudo-slug selects the uncategorized bot.
	if out := listJSON(t, BotsListOptions{Paths: []string{dir}, Categories: []string{"uncategorized"}}); !strings.Contains(out, `"bare"`) || strings.Contains(out, `"revi"`) {
		t.Errorf("uncategorized filter wrong:\n%s", out)
	}
}

func TestBotsList_TagFilterIsAND(t *testing.T) {
	dir := writeTaxonomyBots(t)
	one := listJSON(t, BotsListOptions{Paths: []string{dir}, Tags: []string{"security"}})
	if strings.Contains(one, `"revi"`) {
		t.Errorf("revi carries no security tag but matched:\n%s", one)
	}
	both := listJSON(t, BotsListOptions{Paths: []string{dir}, Tags: []string{"code-review", "read-only"}})
	if !strings.Contains(both, `"revi"`) || strings.Contains(both, `"bare"`) {
		t.Errorf("AND tag filter lost revi or kept bare:\n%s", both)
	}
}

func TestBotsList_FormatTreeGroupsAndIndentsPresets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "revi", "manifest.yaml"), "name: revi\ndisplay_name: Revi\ncategory: verify\n")
	writeFile(t, filepath.Join(dir, "revi", "main.bot"), "agent x:\n  model: \"test\"\n")
	writeFile(t, filepath.Join(dir, "revi", "presets", "strict.md"), "---\nname: strict\ndescription: strict mode.\n---\nprompt\n")
	writeFile(t, filepath.Join(dir, "bare", "manifest.yaml"), "name: bare\n")
	writeFile(t, filepath.Join(dir, "bare", "main.bot"), "agent x:\n  model: \"test\"\n")

	var buf bytes.Buffer
	if err := BotsList(BotsListOptions{Paths: []string{dir}, Format: "tree"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"Verify — judge the code, touch nothing (1)",
		"Revi · revi",
		"· strict", // preset leaf under its bot
		"Uncategorized — visible, never hidden (1)",
		"bare · bare",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tree output missing %q:\n%s", want, out)
		}
	}
	// The preset leaf is indented DEEPER than its bot line.
	botAt := strings.Index(out, "Revi · revi")
	presetAt := strings.Index(out, "· strict")
	if botAt < 0 || presetAt < botAt {
		t.Errorf("preset leaf not under its bot:\n%s", out)
	}
}
