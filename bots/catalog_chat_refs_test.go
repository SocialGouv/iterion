package bots

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bundle's `chat:` block names workflow nodes, vars and schema fields BY
// NAME, and nothing in the toolchain joins those names to the compiled
// main.bot: `validateChatSurface` (pkg/bundle/chat.go) checks the block's
// SHAPE — kind in the closed set, a human node names a text_field, a
// launcher implies a seed_var — and pkg/bundlelint predates the block
// entirely.
//
// The failure this leaves open is silent in the worst way. Rename the human
// node in main.bot (`chat` → `reply`) and the bundle still validates, still
// packs, still ships: the stale `chat` mapping matches nothing, the real
// node is unmapped, and the studio degrades an unmapped node to a progress
// banner (studio/src/lib/whats-next/messagesFromEvents.ts). The dock looks
// alive and swallows every operator message — no compile error, no bundle
// lint finding, no failing test. Exactly the class this package's skill-ref
// guard (catalog_skill_refs_test.go) already covers for `skills:`.
//
// So: for every catalog bundle that declares a chat surface, cross-check
// the block against its compiled workflow —
//
//   - every `chat.nodes` key is a node that EXISTS in main.bot;
//   - a node the manifest calls `kind: human` really is a human node, and
//     its `text_field` is a field of that node's answer (output) schema —
//     the field the operator's typed text lands in;
//   - `seed_var` and every `launcher_vars` entry name a declared workflow
//     var — the composer writes into them verbatim, so an undeclared one
//     discards the operator's first message.
//
// The lint-level version of this guard — a C2xx in pkg/bundlelint, so
// `iterion validate` catches it for ANY bundle, not just the catalog — is
// tracked in #485. This test pins the shipped contracts until then.
func TestCatalogChatManifestsMatchWorkflow(t *testing.T) {
	teamBots, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	demoBots, err := filepath.Glob("../examples/*/main.bot")
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	mainBots := append(teamBots, demoBots...)
	if len(mainBots) == 0 {
		t.Fatal("no catalog bots found under bots/*/main.bot or examples/*/main.bot")
	}

	checked := 0
	for _, mainBot := range mainBots {
		dir := filepath.Dir(mainBot)
		manifestPath := filepath.Join(dir, "manifest.yaml")
		if _, statErr := os.Stat(manifestPath); statErr != nil {
			continue // loose .bot — nothing to cross-check
		}

		m, err := bundle.LoadManifest(manifestPath)
		if err != nil {
			t.Errorf("%s: load manifest: %v", manifestPath, err)
			continue
		}
		if m == nil || m.Chat == nil {
			continue // not a conversational bundle — nothing to cross-check
		}

		src, err := os.ReadFile(mainBot)
		if err != nil {
			t.Errorf("%s: read: %v", mainBot, err)
			continue
		}
		pr := parser.Parse(mainBot, string(src))
		if pr.File == nil {
			continue // parse failure is another test's job
		}
		cr := ir.Compile(pr.File)
		w := cr.Workflow
		if w == nil {
			continue // compile failure is another test's job
		}
		checked++

		for _, id := range sortedChatNodeIDs(m.Chat.Nodes) {
			n := m.Chat.Nodes[id]
			node, ok := w.Nodes[id]
			if !ok {
				t.Errorf("%s: chat: maps node %q, but %s compiles no such node.\n"+
					"The studio renders an unmapped node as a progress banner: if %q was "+
					"the operator's turn, the dock looks alive and swallows every message.",
					manifestPath, id, mainBot, id)
				continue
			}
			if n.Kind != bundle.ChatNodeHuman {
				continue
			}
			human, ok := node.(*ir.HumanNode)
			if !ok {
				t.Errorf("%s: chat: node %q is kind human, but the workflow node is a %s — "+
					"it can never pause for the operator",
					manifestPath, id, node.NodeKind())
				continue
			}
			if n.TextField == "" {
				continue // shape validation already rejects this; don't cascade
			}
			schema := w.Schemas[human.OutputSchema]
			if schema == nil {
				t.Errorf("%s: chat: node %q collects text_field %q, but the human node "+
					"declares no output schema for it to land in",
					manifestPath, id, n.TextField)
				continue
			}
			found := false
			for _, f := range schema.Fields {
				if f.Name == n.TextField {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: chat: node %q collects text_field %q, but answer schema %q "+
					"has no such field — the operator's reply would have nowhere to land",
					manifestPath, id, n.TextField, human.OutputSchema)
			}
		}

		// The composer writes the first message and launcher answers into
		// these vars VERBATIM; an undeclared name is dropped silently.
		if v := m.Chat.SeedVar; v != "" {
			if _, ok := w.Vars[v]; !ok {
				t.Errorf("%s: chat: seed_var %q is not a declared workflow var — "+
					"the operator's first message would be discarded", manifestPath, v)
			}
		}
		for _, v := range m.Chat.LauncherVars {
			if _, ok := w.Vars[v.Name]; !ok {
				t.Errorf("%s: chat: launcher var %q is not a declared workflow var — "+
					"the launcher would collect it and the run would never see it",
					manifestPath, v.Name)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no catalog chat manifest found — copilot and whats-next both declare one; the glob or the layout changed")
	}
	t.Logf("cross-checked %d chat manifest(s) against their compiled workflows", checked)
}

// sortedChatNodeIDs keeps failure output deterministic; Go map order would
// report a different one of several offending nodes each run.
func sortedChatNodeIDs(m map[string]bundle.ChatNode) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
