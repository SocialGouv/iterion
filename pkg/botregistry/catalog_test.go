package botregistry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/mdcode"
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

func TestRenderCatalogBlock_GroupsByCategoryUncategorizedLast(t *testing.T) {
	entries := []EntryWithSchema{
		{Entry: Entry{Name: "zeta", DisplayName: "Zeta", Enabled: true, IsBundleDir: true,
			Category: "steer", Tags: []string{"conversational"}}},
		{Entry: Entry{Name: "alpha", DisplayName: "Al", Enabled: true, IsBundleDir: true,
			Category: "verify"}},
		{Entry: Entry{Name: "bravo", Enabled: true, IsBundleDir: true,
			Category: "build"}},
		{Entry: Entry{Name: "bare", Enabled: true, IsBundleDir: true}},
	}
	block := RenderCatalogBlock(entries, "", "")

	// Group headings in canonical order, Uncategorized visibly last.
	buildAt := strings.Index(block, "### Build — ship new capability")
	verifyAt := strings.Index(block, "### Verify — judge the code, touch nothing")
	steerAt := strings.Index(block, "### Steer — judge the direction, converse")
	uncatAt := strings.Index(block, "### Uncategorized — visible, never hidden")
	if buildAt < 0 || verifyAt < 0 || steerAt < 0 || uncatAt < 0 {
		t.Fatalf("missing a group heading\n---\n%s", block)
	}
	if buildAt >= verifyAt || verifyAt >= steerAt || steerAt >= uncatAt {
		t.Errorf("group order wrong: build@%d verify@%d steer@%d uncat@%d", buildAt, verifyAt, steerAt, uncatAt)
	}
	// Empty canonical groups are skipped in the routing document.
	if strings.Contains(block, "### Document —") {
		t.Errorf("empty category Document printed\n---\n%s", block)
	}
	// Cards carry their tags; the persona table carries the category column.
	if !strings.Contains(block, "- **Tags**: conversational") {
		t.Errorf("card missing its Tags line\n---\n%s", block)
	}
	if !strings.Contains(block, "| Zeta | `zeta` | steer |") {
		t.Errorf("persona table missing the category column\n---\n%s", block)
	}
	// The uncategorized bot stays in the document.
	if !strings.Contains(block, "### `bare`") {
		t.Errorf("uncategorized bot dropped\n---\n%s", block)
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
			catalogGeneratedBegin+"\n"+catalogGeneratedEnd+"\n\nPREAMBLE-BOTTOM\n\n"+
			"See [backends](../../docs/backends.md).\n")

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
		"| Enably | `enabled-bot` | — |",
		"### `enabled-bot`",
		"use the enabled bot",
		"| Nexie | `whats-next` (this bot) | — |",
		// The static tail's link resolved from the template's own directory
		// at bots/whats-next/; the copy lives one level deeper, so the
		// splice must have climbed it.
		"](../../../docs/backends.md)",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("generated catalog missing %q\n---\n%s", must, got)
		}
	}
	if strings.Contains(got, "](../docs/backends.md)") {
		t.Errorf("generated catalog carries the template-depth link, which names nothing from skills/:\n%s", got)
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

func TestRegenerateWhatsNextCatalog_LooseFileSkipDoesNotBlock(t *testing.T) {
	// The refusal guards the catalog's CONTENT: only bundle dirs (and
	// unreadable dirs, conservatively) can carry a card. A skipped loose
	// .bot file — here a dangling symlink under examples/ — must not
	// freeze catalog regeneration.
	dir := fixtureCatalogWorkspace(t)
	ClearSchemaCache()
	if err := os.Symlink(filepath.Join(dir, "examples", "gone.bot"), filepath.Join(dir, "examples", "dead.bot")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	dest, err := RegenerateWhatsNextCatalog(dir)
	if err != nil {
		t.Fatalf("a loose-file skip must not block regen: %v", err)
	}
	if dest == "" {
		t.Fatal("expected the catalog to regenerate")
	}
}

func TestRegenerateWhatsNextCatalog_ShadowedLooseFileDoesNotBlock(t *testing.T) {
	// A shadowed LOOSE .bot carries no card at all — loose files are
	// filtered out of the catalog, and the name routes to the claimer — so
	// the refusal, which exists to stop a partial list from deleting a
	// bot's row, must not fire on it. It runs at every run start: a second
	// `main.bot` dropped under examples/ would otherwise freeze every
	// regeneration in the workspace.
	dir := fixtureCatalogWorkspace(t)
	ClearSchemaCache()
	stub := "agent x:\n  model: \"test\"\n"
	writeFile(t, filepath.Join(dir, "examples", "demo-a", "main.bot"), stub)
	writeFile(t, filepath.Join(dir, "examples", "demo-b", "main.bot"), stub)

	// The premise, asserted rather than assumed: this workspace really does
	// hold a shadowed source for the regen to pass over.
	_, diags, err := ListWithDiagnostics(ListOptions{Paths: DefaultPaths(dir)})
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 1 || diags[0].Kind != DiscoveryErrorShadowed {
		t.Fatalf("diags = %#v, want exactly one shadowed diagnostic", diags)
	}

	dest, err := RegenerateWhatsNextCatalog(dir)
	if err != nil {
		t.Fatalf("a shadowed source must not block regen: %v", err)
	}
	if dest == "" {
		t.Fatal("expected the catalog to regenerate")
	}
}

func TestRegenerateWhatsNextCatalog_ShadowedBundleBlocks(t *testing.T) {
	// The other side of the same classification: a shadowed BUNDLE is the
	// one shape whose card nothing else carries. Cards are built from
	// bundles, so when a loose `main.bot` (or any .bot) takes the bundle's
	// name first, the bundle leaves the list and regenerating rewrites the
	// COMMITTED catalog with its row and card deleted — in silence, at every
	// run start and on every studio bot-metadata save. That deletion is what
	// this refusal exists to stop, so the kind alone cannot clear it.
	dir := fixtureCatalogWorkspace(t)
	ClearSchemaCache()

	// The committed state: a catalog regenerated while the workspace was
	// clean, carrying the bundle's card.
	dest, err := RegenerateWhatsNextCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(committed), "`enabled-bot`") {
		t.Fatalf("the committed catalog must carry the bundle's card to begin with:\n%s", committed)
	}

	// A loose .bot claiming the bundle's name. It sorts first inside bots/,
	// so it is the one that holds the name.
	ClearSchemaCache()
	writeFile(t, filepath.Join(dir, "bots", "aaa.bot"),
		"## ---\n## name: enabled-bot\n## ---\nagent x:\n  model: \"test\"\n")

	// The premise, asserted rather than assumed: the BUNDLE is the source
	// that lost the name, and the path the diagnostic reports is relative to
	// the WORKSPACE — which is what the refusal must re-anchor it on, the
	// process's own directory being nowhere near it. The whole
	// classification rests on that pairing, so it is pinned here.
	_, diags, err := ListWithDiagnostics(ListOptions{Paths: DefaultPaths(dir), Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 1 || diags[0].Kind != DiscoveryErrorShadowed || diags[0].Path != "bots/enabled-bot" {
		t.Fatalf("diags = %#v, want exactly one shadowed diagnostic naming the bundle directory, workspace-relative", diags)
	}
	if info, serr := os.Stat(resolveDiagPath(dir, diags[0].Path)); serr != nil || !info.IsDir() {
		t.Fatalf("re-anchoring %q on %q must find the bundle directory: %v", diags[0].Path, dir, serr)
	}

	if _, err := RegenerateWhatsNextCatalog(dir); err == nil {
		t.Error("regenerating over a shadowed BUNDLE must refuse: its card is in no other entry, so the committed row would be deleted")
	} else if !strings.Contains(err.Error(), "enabled-bot") {
		t.Errorf("the refusal must name the source: %v", err)
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(committed) {
		t.Errorf("the committed catalog was rewritten anyway:\n--- before ---\n%s\n--- after ---\n%s", committed, after)
	}
}

// The template is spliced verbatim one directory deeper than where it is
// authored; a relative target that resolved next to the template names
// nothing next to the copy. The re-anchor is what keeps the spliced link
// pointing at the file it named; its mutation to identity reddens every
// case that carries a relative target — and the catalog-freshness gate
// reddens the whole pipeline, since regen would re-emit the mis-anchored
// form the tree no longer carries.
func TestReanchorForDepthRewritesRelativeTargets(t *testing.T) {
	const templateDir = "bots/whats-next"
	const skillsDir = "bots/whats-next/skills"
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{
			// The static template's own tail link, spliced one level deeper.
			name: "a template-root relative link",
			text: "[docs/backends.md](../../docs/backends.md)",
			want: "[docs/backends.md](../../../docs/backends.md)",
		},
		{
			name: "a fragment rides its target",
			text: "[the gate](../merge-gate.md#the-gate)",
			want: "[the gate](../../merge-gate.md#the-gate)",
		},
		{
			name: "a bare fragment keeps naming this document",
			text: "the [decision tree](#decision-tree) below",
			want: "the [decision tree](#decision-tree) below",
		},
		{
			name: "scheme, protocol-relative and /-prefixed targets pass",
			text: "[gh](https://github.com/x), [cdn](//cdn/x), [abs](/docs/a.md)",
			want: "[gh](https://github.com/x), [cdn](//cdn/x), [abs](/docs/a.md)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reanchorForDepth(tc.text, templateDir, skillsDir); got != tc.want {
				t.Errorf("reanchorForDepth(%q)\n  got  %s\n  want %s", tc.text, got, tc.want)
			}
		})
	}
	if got := reanchorForDepth("[a](b.md)", "same/dir", "same/dir"); got != "[a](b.md)" {
		t.Errorf("same-directory splice must be a no-op, got %s", got)
	}
}

// assertNoPhantomFence fails when a rendered catalog block leaves a fenced
// code block OPEN — which turns every line of the document after it into
// code — or when one of proseLines, which the block means as prose, is
// nonetheless read as code.
func assertNoPhantomFence(t *testing.T, block string, proseLines ...string) {
	t.Helper()
	// The lines a renderer reads, which is the only reading that matters
	// here: a value's lone carriage return breaks its line on the page, so a
	// witness splitting on line feeds alone would call a phantom fence sound.
	lines := mdcode.Lines(block)
	var fence mdcode.FenceScanner
	for _, line := range lines {
		fence.Code(line)
	}
	if fence.Open() {
		t.Errorf("the rendered block leaves a fenced block OPEN — everything spliced after it reads as code:\n%s", block)
	}
	masked := mdcode.Lines(mdcode.Mask(block))
	for _, want := range proseLines {
		found := false
		for i, line := range lines {
			if strings.TrimSpace(line) != want {
				continue
			}
			found = true
			if strings.TrimSpace(masked[i]) == "" {
				t.Errorf("line %d %q reads as CODE in the rendered block:\n%s", i+1, want, block)
			}
		}
		if !found {
			t.Errorf("line %q is not in the rendered block:\n%s", want, block)
		}
	}
}

// TestRenderCatalogCardDescriptionOpensNoFence pins the splice against a
// description whose word wrap starts a line with a fence marker: the card
// must carry it as the prose it is, never as a block opener that swallows
// the cards below.
func TestRenderCatalogCardDescriptionOpensNoFence(t *testing.T) {
	const wrapped = "Posts the findings onto the pull request as inline comments with one-click\n" +
		"```suggestion blocks (GitHub / GitLab / Forgejo). Never edits code."
	entries := []EntryWithSchema{
		{Entry: Entry{
			Name: "alpha", Description: wrapped,
			Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
		}},
		{Entry: Entry{
			Name: "omega", Description: "The card that follows.",
			Enabled: true, IsBundleDir: true, Path: "/w/bots/omega",
		}},
	}
	assertNoPhantomFence(t, RenderCatalogBlock(entries, "", "/w"),
		"### `omega`", "The card that follows.")
}

// TestRenderCatalogCardWhenToUseOpensNoFence is the same guarantee for the
// OTHER manifest field the card splices as a block. Two spaces of list
// indentation do not stop a fence: CommonMark opens one up to three columns
// in.
func TestRenderCatalogCardWhenToUseOpensNoFence(t *testing.T) {
	const wrapped = "Use when you want a branch REVIEWED and its findings posted as\n" +
		"```suggestion fixes — but NOT auto-fixed."
	entries := []EntryWithSchema{
		{Entry: Entry{
			Name: "alpha", WhenToUse: wrapped,
			Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
		}},
		{Entry: Entry{
			Name: "omega", Description: "The card that follows.",
			Enabled: true, IsBundleDir: true, Path: "/w/bots/omega",
		}},
	}
	assertNoPhantomFence(t, RenderCatalogBlock(entries, "", "/w"),
		"### `omega`", "The card that follows.")
}

// TestTheWhenToUseBlockKeepsTheAuthorsLineBytes holds the block road of
// when_to_use to the author's bytes: it INDENTS each line and alters nothing
// in it.
//
// Two trailing spaces are a hard line break — pandoc 3.1.3 gfm renders
// "first line  \nsecond line" as `first line<br />second line` and the
// trimmed form as one run of prose — and trailing whitespace inside the
// field's own fenced example is the example's own bytes, which a
// `diff`/`suggestion` block cannot afford to lose. The fence rule needs
// neither gone: a closer's trailing spaces are trimmed where it is read, and
// an info string is read off what follows the marker.
func TestTheWhenToUseBlockKeepsTheAuthorsLineBytes(t *testing.T) {
	for _, tc := range []struct{ name, when, want string }{
		{
			name: "two trailing spaces stay a hard line break",
			when: "first line  \nsecond line",
			want: "  first line  \n  second line\n",
		},
		{
			name: "trailing whitespace inside the author's own fence is the example",
			when: "example:\n```diff\n-old   \n+new\t\n```",
			want: "  example:\n  ```diff\n  -old   \n  +new\t\n  ```\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := []EntryWithSchema{{Entry: Entry{
				Name: "alpha", WhenToUse: tc.when,
				Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
			}}}
			block := RenderCatalogBlock(entries, "", "/w")
			if !strings.Contains(block, tc.want) {
				t.Errorf("the card rewrote the author's lines:\n  want the block to carry %q\n  got\n%s", tc.want, block)
			}
		})
	}
}

// TestRenderCatalogBlockFieldsOpenNoFenceWhateverTheLineEnding holds the two
// block fields against the line breaks a renderer sees and a line-feed split
// does not — a lone carriage return and a carriage return with a line feed —
// and against a closing fence a tab pushed past the three columns a closer
// may take, which closes nothing on the page.
//
// The second bundle ships a BALANCED fenced block, which is what makes this
// a test of the per-field escape rather than of the block-level belt: its
// closing delimiter closes whatever the first bundle left open, so the belt
// running over the whole block sees a balanced document and repairs nothing.
// Every line between the two — the first card's bullets, the second card's
// heading — is then code on the page.
//
// The last shape carries NO line break, which is the road when_to_use takes
// when it is spliced inside its bullet rather than indented under it: a value
// with no break cannot start a line, so a delimiter run opening it is
// mid-line text, and the claim is that this road needs no repair — not that
// nobody drives it.
func TestRenderCatalogBlockFieldsOpenNoFenceWhateverTheLineEnding(t *testing.T) {
	for _, hostile := range []struct{ name, value string }{
		{"a lone carriage return before the marker", "one-click\r```suggestion blocks (GitHub)."},
		{"a carriage return and a line feed", "one-click\r\n```suggestion blocks (GitHub)."},
		{"a closer a tab pushed four columns in", "shows:\n```suggestion\n- the replacement\n\t```"},
		{"a carriage return inside a longer field", "reads:\rshows:\r```suggestion\rand stops"},
		{"a single line that STARTS with a fence marker", "```suggestion blocks (GitHub) are what it posts."},
	} {
		for _, field := range []struct {
			name string
			set  func(*EntryWithSchema, string)
		}{
			{"description", func(e *EntryWithSchema, v string) { e.Description = v }},
			{"when_to_use", func(e *EntryWithSchema, v string) { e.WhenToUse = v }},
		} {
			t.Run(field.name+"/"+hostile.name, func(t *testing.T) {
				alpha := EntryWithSchema{Entry: Entry{
					Name: "alpha", Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
				}}
				field.set(&alpha, hostile.value)
				omega := EntryWithSchema{Entry: Entry{
					Name: "omega", Description: "Reads a workflow:\n\n```yaml\nnodes: []\n```\n\nThe card that follows.",
					Enabled: true, IsBundleDir: true, Path: "/w/bots/omega",
				}}
				assertNoPhantomFence(t, RenderCatalogBlock([]EntryWithSchema{alpha, omega}, "", "/w"),
					"### `omega`", "The card that follows.")
			})
		}
	}
}

// separatorPipes counts the pipes of a row that CUT it into cells, by the
// renderer's rule: a pipe is escaped when an odd number of backslashes
// precedes it. The parity is the point — a doubly-escaped `\\|` is an escaped
// backslash followed by a LIVE separator, so counting "not preceded by a
// backslash" would call a twice-escaped cell sound (measured on pandoc 3.1.3
// gfm: the row splits exactly as if nothing had been escaped).
func separatorPipes(row string) int {
	n := 0
	for i := 0; i < len(row); i++ {
		if row[i] != '|' {
			continue
		}
		slashes := 0
		for j := i - 1; j >= 0 && row[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			n++
		}
	}
	return n
}

// TestPersonaTableCellsKeepTheirColumn pins the three cells of the routing
// table against the one byte a cell cannot carry: its own separator. A row is
// cut on its unescaped pipes before any of its text is read, so a pipe in a
// persona moves the persona's tail into the `assignee` column the dispatcher
// routes on — and drops the row's last cell off the table. Measured on both
// renderers of this file (GitHub's /markdown API and pandoc 3.1.3 gfm,
// 2026-10-12).
func TestPersonaTableCellsKeepTheirColumn(t *testing.T) {
	for _, tc := range []struct {
		field string
		set   func(*EntryWithSchema, string)
	}{
		{"display_name", func(e *EntryWithSchema, v string) { e.DisplayName = v }},
		{"name", func(e *EntryWithSchema, v string) { e.Name = v }},
		{"category", func(e *EntryWithSchema, v string) { e.Category = v }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			e := EntryWithSchema{Entry: Entry{
				Name: "alpha", DisplayName: "Al", Category: "verify",
				Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
			}}
			tc.set(&e, "Revi | the reviewer")
			block := RenderCatalogBlock([]EntryWithSchema{e}, "", "/w")
			rows := 0
			for _, line := range mdcode.Lines(block) {
				if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") ||
					strings.HasPrefix(line, "| Persona") {
					continue
				}
				rows++
				if n := separatorPipes(line); n != 4 {
					t.Errorf("%s put %d cell separator(s) in a three-column row (4 expected): %s", tc.field, n, line)
				}
				if !strings.Contains(line, `Revi \| the reviewer`) {
					t.Errorf("%s lost its pipe instead of escaping it: %s", tc.field, line)
				}
			}
			if rows != 1 {
				t.Fatalf("expected one persona row, found %d:\n%s", rows, block)
			}
		})
	}
}

// TestRenderCatalogCardKeepsABalancedFence pins the other half of the rule:
// a description that opens a fence AND closes it wrote that block on
// purpose, and the splice carries it unchanged.
func TestRenderCatalogCardKeepsABalancedFence(t *testing.T) {
	const fenced = "Reads a workflow file:\n\n```yaml\nnodes:\n  - id: review\n```\n\nand runs it."
	entries := []EntryWithSchema{
		{Entry: Entry{
			Name: "alpha", Description: fenced,
			Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
		}},
	}
	block := RenderCatalogBlock(entries, "", "/w")
	if !strings.Contains(block, "```yaml\nnodes:\n  - id: review\n```") {
		t.Errorf("a description's own balanced fence was altered:\n%s", block)
	}
	assertNoPhantomFence(t, block, "and runs it.")
}

// TestReanchorForDepthLeavesQuotedTargetsAlone pins the re-anchor to
// internal/mdcode's "which bytes are code" rule: a `](target)` a card
// QUOTES — inside a fenced block or an inline span — is a form the document
// shows, not a link it makes, and rewriting it corrupts the very example the
// page is teaching.
func TestReanchorForDepthLeavesQuotedTargetsAlone(t *testing.T) {
	const templateDir = "bots/whats-next"
	const skillsDir = "bots/whats-next/skills"
	const in = "```md\n[quoted](../../docs/backends.md)\n```\n\n" +
		"An inline span: `[quoted](../../docs/backends.md)`\n\n" +
		"[a real link](../../docs/backends.md)\n"
	const want = "```md\n[quoted](../../docs/backends.md)\n```\n\n" +
		"An inline span: `[quoted](../../docs/backends.md)`\n\n" +
		"[a real link](../../../docs/backends.md)\n"
	if got := reanchorForDepth(in, templateDir, skillsDir); got != want {
		t.Errorf("reanchorForDepth rewrote a quoted target\n  got  %q\n  want %q", got, want)
	}
}

// TestReanchorForDepthStopsAtTheEndOfTheLine pins the link grammar to ONE
// line. A card can write a stray `](` — the catalogue documents link forms —
// and a target that may run past the newline lets the match reach across the
// document for the next parenthesis and replace everything between with a
// path.
func TestReanchorForDepthStopsAtTheEndOfTheLine(t *testing.T) {
	const templateDir = "bots/whats-next"
	const skillsDir = "bots/whats-next/skills"
	const in = "a stray ](form in a card's prose\n\n[a real link](../../docs/backends.md)\n"
	const want = "a stray ](form in a card's prose\n\n[a real link](../../../docs/backends.md)\n"
	if got := reanchorForDepth(in, templateDir, skillsDir); got != want {
		t.Errorf("reanchorForDepth crossed a newline to close a link\n  got  %q\n  want %q", got, want)
	}
}

// TestRenderCatalogCardInlineFieldsOpenNoFence holds the other half of the
// card's rule: every manifest value spliced INSIDE a line stays on that
// line, so no delimiter run of it can reach column 0 and open a block over
// the cards below.
//
// Each case carries the fence marker on a SECOND line of the value, which is
// the only way an inline splice can start a line: a single-line value cannot
// falsify the claim, and the fields reached by it are exactly the ones a
// manifest can set — `display_name` and `category` are spliced into the
// persona table ABOVE every card, the others into their own bullet. A field
// added to the card without the same treatment reddens here.
func TestRenderCatalogCardInlineFieldsOpenNoFence(t *testing.T) {
	// Two lines, then three, because the trailing backtick a path or a var
	// name is wrapped in saves a two-line value by accident: `x\n```y` is no
	// fence (a backtick fence's info string may hold no backtick), while
	// `x\n```y\nz` is one.
	for _, hostile := range []string{
		"post\n```suggestion onto the forge",
		"post\n```suggestion\nonto the forge",
		"post\n~~~suggestion\nonto the forge",
	} {
		for _, tc := range []struct {
			field string
			set   func(*EntryWithSchema, string)
		}{
			{"display_name", func(e *EntryWithSchema, v string) { e.DisplayName = v }},
			{"category", func(e *EntryWithSchema, v string) { e.Category = v }},
			{"triggers", func(e *EntryWithSchema, v string) { e.Triggers = []string{v} }},
			{"tags", func(e *EntryWithSchema, v string) { e.Tags = []string{v} }},
			{"capabilities", func(e *EntryWithSchema, v string) { e.Capabilities = []string{v} }},
			{"vars", func(e *EntryWithSchema, v string) {
				e.Vars = &VarsBlock{Fields: []*VarField{{Name: v, Type: "string"}}}
			}},
			{"path", func(e *EntryWithSchema, v string) { e.Path = "/w/bots/" + v }},
			{"name", func(e *EntryWithSchema, v string) { e.Name = v }},
		} {
			name := fmt.Sprintf("%s/%d-line", tc.field, strings.Count(hostile, "\n")+1)
			t.Run(name, func(t *testing.T) {
				render := func(value string) string {
					alpha := EntryWithSchema{Entry: Entry{
						Name: "alpha", Description: "The first card.",
						Enabled: true, IsBundleDir: true, Path: "/w/bots/alpha",
					}}
					tc.set(&alpha, value)
					omega := EntryWithSchema{Entry: Entry{
						Name: "omega", Description: "The card that follows.",
						Enabled: true, IsBundleDir: true, Path: "/w/bots/omega",
					}}
					return RenderCatalogBlock([]EntryWithSchema{alpha, omega}, "", "/w")
				}
				block := render(hostile)
				assertNoPhantomFence(t, block, "### `omega`", "The card that follows.")
				// The value is spliced inside a line, so its line breaks
				// change nothing in the document: the card reads as if the
				// author had written the same words on one line.
				if collapsed := render(strings.Join(strings.Fields(hostile), " ")); block != collapsed {
					t.Errorf("a line break in %s changed the rendering\n  got  %q\n  want %q", tc.field, block, collapsed)
				}
			})
		}
	}
}

// TestReanchorForDepthKeepsTheBytesOfAQuotedDestination pins the one thing
// reading the document through the mask must not do: a `)` carried inside an
// inline span is blanked, so the link grammar runs past the parenthesis that
// ends the destination and the rewrite would re-emit a resolved path in
// place of the bytes the card wrote. Only a destination the mask agrees is
// prose end to end is re-anchored.
func TestReanchorForDepthKeepsTheBytesOfAQuotedDestination(t *testing.T) {
	const templateDir = "bots/whats-next"
	const skillsDir = "bots/whats-next/skills"
	for _, in := range []string{
		"a link [x](a`)`/../b) in a card's prose\n",
		"a link [x](a`)`//b) in a card's prose\n",
	} {
		got := reanchorForDepth(in, templateDir, skillsDir)
		if got != in {
			t.Errorf("reanchorForDepth rewrote a destination the mask reads as part code\n  got  %q\n  want %q", got, in)
		}
	}
	// The ordinary destination on the same shape of line still moves.
	const plain = "a link [x](../../docs/backends.md) in a card's prose\n"
	const want = "a link [x](../../../docs/backends.md) in a card's prose\n"
	if got := reanchorForDepth(plain, templateDir, skillsDir); got != want {
		t.Errorf("a plain relative destination was not re-anchored\n  got  %q\n  want %q", got, want)
	}
}
