package botregistry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderCatalogBlock_FiltersDisabledShowsWhenToUseAndVars(t *testing.T) {
	entries := []EntryWithSchema{
		{
			Entry: Entry{
				Name: "alpha", DisplayName: "Al", Description: "Alpha bot.",
				WhenToUse: "use for alpha work", Triggers: []string{"a"},
				Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
			},
			Vars: &VarsBlock{Fields: []*VarField{
				{Name: "feature_prompt", Type: "string"},                             // no default → required
				{Name: "workspace_dir", Type: "string", Default: &Literal{Raw: "."}}, // has default
			}},
		},
		{
			Entry: Entry{
				Name: "off", DisplayName: "Offy", Description: "Disabled bot.",
				Enabled: false, IsBundleDir: true, Path: "/w/bots/off",
			},
		},
	}
	block := RenderCatalogBlock(entries, "alpha", "/w")

	for _, want := range []string{
		"### `alpha`",
		"use for alpha work",
		"`feature_prompt` (string, required)",
		"`workspace_dir` (string)",
		"`alpha` (this bot)",
		"bots/alpha",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q\n---\n%s", want, block)
		}
	}
	// workspace_dir has a default → must NOT be marked required.
	if strings.Contains(block, "`workspace_dir` (string, required)") {
		t.Errorf("var with a default wrongly marked required\n%s", block)
	}
	// Disabled bot is omitted entirely.
	if strings.Contains(block, "off") || strings.Contains(block, "Offy") {
		t.Errorf("disabled bot leaked into the catalog block\n%s", block)
	}
}

// fixtureCatalogWorkspace builds a workspace with the whats-next catalog
// template, two extra bundles (one disabled), and a loose example file.
func fixtureCatalogWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := "agent x:\n  model: \"test\"\n"

	writeFile(t, filepath.Join(dir, "bots", "whats-next", "manifest.yaml"),
		"name: whats-next\ndisplay_name: Nexie\ndescription: Orchestrator.\n")
	writeFile(t, filepath.Join(dir, "bots", "whats-next", "main.bot"), stub)
	writeFile(t, filepath.Join(dir, "bots", "whats-next", catalogStaticName),
		"---\nname: iterion-bot-catalog\n---\nPREAMBLE-TOP\n\n"+
			catalogGeneratedBegin+"\n"+catalogGeneratedEnd+"\n\nPREAMBLE-BOTTOM\n")

	writeFile(t, filepath.Join(dir, "bots", "enabled-bot", "manifest.yaml"),
		"name: enabled-bot\ndisplay_name: Enably\ndescription: An enabled bot.\nwhen_to_use: use the enabled bot\n")
	writeFile(t, filepath.Join(dir, "bots", "enabled-bot", "main.bot"), stub)

	writeFile(t, filepath.Join(dir, "bots", "disabled-bot", "manifest.yaml"),
		"name: disabled-bot\ndisplay_name: Offy\ndescription: A disabled bot.\nenabled: false\n")
	writeFile(t, filepath.Join(dir, "bots", "disabled-bot", "main.bot"), stub)

	writeFile(t, filepath.Join(dir, "examples", "loose.bot"),
		"## ---\n## name: loose-demo\n## ---\n"+stub)
	return dir
}

func TestRegenerateWhatsNextCatalog_SplicesFiltersAndPreservesStatic(t *testing.T) {
	dir := fixtureCatalogWorkspace(t)
	ClearSchemaCache()

	dest, err := RegenerateWhatsNextCatalog(dir)
	if err != nil {
		t.Fatalf("RegenerateWhatsNextCatalog: %v", err)
	}
	want := filepath.Join(dir, "bots", "whats-next", "skills", catalogGeneratedName)
	if dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	for _, must := range []string{
		"PREAMBLE-TOP",
		"PREAMBLE-BOTTOM",
		catalogGeneratedBegin,
		catalogGeneratedEnd,
		"| Enably | `enabled-bot` |",
		"### `enabled-bot`",
		"use the enabled bot",
		"`whats-next` (this bot)",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("generated catalog missing %q\n---\n%s", must, got)
		}
	}
	// Disabled bundle and loose demo file must NOT appear.
	for _, absent := range []string{"disabled-bot", "Offy", "loose-demo"} {
		if strings.Contains(got, absent) {
			t.Errorf("generated catalog should not contain %q\n---\n%s", absent, got)
		}
	}
}

func TestRegenerateWhatsNextCatalog_NoOpWithoutTemplate(t *testing.T) {
	dir := t.TempDir()
	// A bundle but no catalog static template anywhere.
	writeFile(t, filepath.Join(dir, "bots", "solo", "manifest.yaml"), "name: solo\n")
	writeFile(t, filepath.Join(dir, "bots", "solo", "main.bot"), "agent x:\n  model: \"test\"\n")
	dest, err := RegenerateWhatsNextCatalog(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest != "" {
		t.Errorf("expected no-op (empty dest), got %q", dest)
	}
}

func TestList_AppliesOverlayAndKeepsDisabled(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bots", "b1", "manifest.yaml"), "name: b1\n")
	writeFile(t, filepath.Join(dir, "bots", "b1", "main.bot"), "agent x:\n  model: \"test\"\n")
	if err := SetOverlayEnabled(dir, "b1", boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	entries, err := List(ListOptions{Paths: DefaultPaths(dir), Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var found *Entry
	for i := range entries {
		if entries[i].Name == "b1" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("b1 should still be listed (disabled bots are returned, not dropped)")
	}
	if found.Enabled {
		t.Error("overlay disable should have resolved b1.Enabled=false")
	}
	if !found.IsBundleDir {
		t.Error("b1 should be flagged as a bundle dir")
	}
}

func TestRegenerateWhatsNextCatalog_RefusesPartialDiscovery(t *testing.T) {
	// A malformed bundle beside the catalog owner: regenerating from the
	// partial list would silently delete the skipped bot's card from the
	// committed routing file. Regen must refuse and leave the file as-is.
	dir := fixtureCatalogWorkspace(t)
	ClearSchemaCache()
	writeFile(t, filepath.Join(dir, "bots", "broken-chat", "manifest.yaml"), `name: broken-chat
chat:
  nodes:
    chat:
      kind: human
`)
	writeFile(t, filepath.Join(dir, "bots", "broken-chat", "main.bot"), "agent x:\n  model: \"test\"\n")

	// Pre-existing committed catalog the regen must NOT touch.
	generated := filepath.Join(dir, "bots", "whats-next", "skills", catalogGeneratedName)
	writeFile(t, generated, "COMMITTED-CATALOG-SENTINEL")

	dest, err := RegenerateWhatsNextCatalog(dir)
	if err == nil {
		t.Fatalf("expected a refusal, got dest=%q", dest)
	}
	if !strings.Contains(err.Error(), "broken-chat") {
		t.Errorf("error = %v, want it to name the malformed bundle", err)
	}
	body, rerr := os.ReadFile(generated)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != "COMMITTED-CATALOG-SENTINEL" {
		t.Errorf("committed catalog was rewritten despite partial discovery:\n%s", body)
	}
}
