package botregistry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestList_SortsAndDedups(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b.bot"), `## ---
## name: zebra
## ---
agent x:
  model: "test"
`)
	writeFile(t, filepath.Join(dir, "a.bot"), `## ---
## name: alpha
## ---
agent y:
  model: "test"
`)
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Name != "alpha" || entries[1].Name != "zebra" {
		t.Errorf("entries not sorted: %v", entries)
	}
}

func TestList_DedupesSameBotAcrossRoots(t *testing.T) {
	// A source bot in bots/ and a stray packed copy under the gitignored
	// .botz/ (a local `iterion bundle pack` artifact) must collapse to ONE
	// catalog entry — not duplicate the card and the routing target.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bots", "review-pr", "manifest.yaml"), "name: review-pr\ndisplay_name: Revi\n")
	writeFile(t, filepath.Join(dir, "bots", "review-pr", "main.bot"), "agent x:\n  model: \"test\"\n")
	writeFile(t, filepath.Join(dir, ".botz", "review-pr", "manifest.yaml"), "name: review-pr\ndisplay_name: Revi\n")
	writeFile(t, filepath.Join(dir, ".botz", "review-pr", "main.bot"), "agent x:\n  model: \"test\"\n")

	entries, err := List(ListOptions{Paths: DefaultPaths(dir)})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	var got Entry
	for _, e := range entries {
		if NormalizeName(e.Name) == "review-pr" {
			count++
			got = e
		}
	}
	if count != 1 {
		t.Fatalf("review-pr should dedupe to 1 entry across bots/ and .botz/, got %d", count)
	}
	// Precedence: the bots/ source wins over the .botz/ stray (root order).
	if !strings.Contains(got.Path, filepath.Join("bots", "review-pr")) {
		t.Errorf("bots/ copy should win precedence, got path %q", got.Path)
	}
}

func TestList_MissingPathIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.bot"), `## ---
## name: x
## ---
`)
	entries, err := List(ListOptions{Paths: []string{dir, "/nonexistent/path/12345"}})
	if err != nil {
		t.Fatalf("missing path should not error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
}

func TestList_IgnoresNonBotFilesInDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "notes.txt"), `## ---
## name: legacy
## ---
`)
	writeFile(t, filepath.Join(dir, "current.bot"), `## ---
## name: current
## ---
`)

	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries: %#v", len(entries), entries)
	}
	if entries[0].Name != "current" {
		t.Errorf("Name = %q, want current", entries[0].Name)
	}
}

func TestList_RejectsNonBotFileRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.txt")
	writeFile(t, path, `## ---
## name: legacy
## ---
`)

	_, err := List(ListOptions{Paths: []string{path}})
	if err == nil {
		t.Fatal("expected non-.bot root to be rejected")
	}
}

func TestList_BundleCarriesDisplayName(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "whats-next")
	writeFile(t, filepath.Join(bundleDir, "manifest.yaml"), `name: whats-next
display_name: Nexie
description: Orchestrator bot.
`)
	writeFile(t, filepath.Join(bundleDir, "main.bot"), `agent x:
  model: "test"
`)
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].DisplayName != "Nexie" {
		t.Errorf("DisplayName = %q, want Nexie (manifest display_name must survive discovery)", entries[0].DisplayName)
	}
}

func TestList_BundleCarriesChatSurface(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "copilot")
	writeFile(t, filepath.Join(bundleDir, "manifest.yaml"), `name: copilot
display_name: Copi
chat:
  seed_var: initial_message
  nodes:
    chat:
      kind: human
      text_field: message
`)
	writeFile(t, filepath.Join(bundleDir, "main.bot"), `agent x:
  model: "test"
`)
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Chat == nil {
		t.Fatalf("discovered chat surface = %#v, want one", entries)
	}
	chat := entries[0].Chat
	if chat.SeedVar != "initial_message" || chat.Nodes["chat"].TextField != "message" {
		t.Fatalf("discovered chat surface = %#v", chat)
	}
}

func TestResolveBotPath_LooseFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "feature_dev.bot")
	writeFile(t, p, `## ---
## name: feature_dev
## ---
`)
	got, err := ResolveBotPath("feature_dev", []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Errorf("ResolveBotPath = %q, want %q", got, p)
	}
}

func TestResolveBotPath_Bundle(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "mybot")
	writeFile(t, filepath.Join(bundleDir, "manifest.yaml"), `name: mybot
description: ""
`)
	main := filepath.Join(bundleDir, "main.bot")
	writeFile(t, main, `agent x:
  model: "test"
`)
	got, err := ResolveBotPath("mybot", []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if got != main {
		t.Errorf("ResolveBotPath = %q, want %q", got, main)
	}
}

func TestResolveBotPath_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveBotPath("ghost", []string{dir})
	if err == nil {
		t.Fatal("expected error for missing bot")
	}
}

func TestDefaultPaths(t *testing.T) {
	got := DefaultPaths("/work")
	want := []string{
		filepath.Join("/work", "bots"),
		filepath.Join("/work", "examples"),
		filepath.Join("/work", ".botz"),
	}
	if len(got) != len(want) {
		t.Fatalf("DefaultPaths len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DefaultPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEntry_MainFile(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "b")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose.bot")
	writeFile(t, loose, `## name: x ##`)

	bundleEntry := Entry{Path: bundleDir, Name: "b"}
	if got := bundleEntry.MainFile(); got != filepath.Join(bundleDir, "main.bot") {
		t.Errorf("bundle MainFile = %q", got)
	}
	looseEntry := Entry{Path: loose, Name: "x"}
	if got := looseEntry.MainFile(); got != loose {
		t.Errorf("loose MainFile = %q", got)
	}
}

func TestWorkdirFromPaths(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"/w/bots", "/w/examples", "/w/.botz"}, "/w"},
		{[]string{"/w/examples"}, "/w"},
		{[]string{"/some/custom/dir"}, ""}, // base not a recognised discovery root
		{nil, ""},
	}
	for _, c := range cases {
		if got := WorkdirFromPaths(c.in); got != c.want {
			t.Errorf("WorkdirFromPaths(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestList_SetsRelPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bots", "b1", "manifest.yaml"), "name: b1\n")
	writeFile(t, filepath.Join(dir, "bots", "b1", "main.bot"), "agent x:\n  model: \"test\"\n")
	entries, err := List(ListOptions{Paths: DefaultPaths(dir), Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].RelPath != "bots/b1" {
		t.Errorf("RelPath = %q, want bots/b1", entries[0].RelPath)
	}
	// Without a workdir, RelPath stays empty.
	bare, _ := List(ListOptions{Paths: DefaultPaths(dir)})
	if bare[0].RelPath != "" {
		t.Errorf("RelPath without workdir = %q, want empty", bare[0].RelPath)
	}
}

func TestList_MalformedBundleDoesNotBlankSiblings(t *testing.T) {
	// One bundle with a malformed chat: block (validateChatSurface rejects a
	// human node with no text_field) next to one valid bundle: the malformed
	// one stays OUT with its diagnostic, the valid one still lists.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good", "manifest.yaml"), "name: good\ndescription: fine\n")
	writeFile(t, filepath.Join(dir, "good", "main.bot"), "agent x:\n  model: \"test\"\n")
	writeFile(t, filepath.Join(dir, "broken-chat", "manifest.yaml"), `name: broken-chat
chat:
  nodes:
    chat:
      kind: human
`)
	writeFile(t, filepath.Join(dir, "broken-chat", "main.bot"), "agent x:\n  model: \"test\"\n")

	entries, diags, err := ListWithDiagnostics(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "good" {
		t.Fatalf("entries = %#v, want only the valid bundle", entries)
	}
	if len(diags) != 1 {
		t.Fatalf("diags = %#v, want one discovery error", diags)
	}
	if !strings.HasSuffix(diags[0].Path, "broken-chat") {
		t.Errorf("diag.Path = %q, want the broken-chat bundle dir", diags[0].Path)
	}
	if !strings.Contains(diags[0].Error, "chat:") {
		t.Errorf("diag.Error = %q, want the chat: validation diagnostic", diags[0].Error)
	}

	// List keeps its two-value contract: valid siblings, no error.
	plain, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 1 || plain[0].Name != "good" {
		t.Fatalf("List = %#v, want only the valid bundle", plain)
	}
}

func TestList_DiagnosticPathIsWorkdirRelative(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bots", "broken", "manifest.yaml"), "name: broken\nschema_version: 9999\n")
	writeFile(t, filepath.Join(dir, "bots", "broken", "main.bot"), "agent x:\n  model: \"test\"\n")
	_, diags, err := ListWithDiagnostics(ListOptions{Paths: DefaultPaths(dir), Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 1 {
		t.Fatalf("diags = %#v, want one", diags)
	}
	if diags[0].Path != "bots/broken" {
		t.Errorf("diag.Path = %q, want workdir-relative bots/broken", diags[0].Path)
	}
}
