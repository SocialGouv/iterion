package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
)

// TestAssistantDependencyBotsLocalizeKeepsTheCatalogConsistentWithDiscovery
// locks the reason the localize path owes no catalog refresh: it lands the
// localized source under the consumer bundle's own subtree, and discovery —
// while it claims the consumer bundle — claims a bundle directory and skips
// its subtree, so the discovered set the catalogue renders is exactly what
// it was before. The claimed-consumer precondition is load-bearing: a
// consumer discovery does not claim has its plugins/<dep> walked, and the
// localized copy then shadows the pinned materialization
// (docs/agents/engine/bot-catalog-regeneration.md records the corner). Both
// halves are asserted, not assumed: neither catalogue's bytes may move
// during the localize — the workspace owner's that the test arms at
// bots/router, and the consumer bundle's own that it arms at
// bots/planner — because a hidden refresh, wholesale or scoped to the
// consumer, would pick up the deliberately stale description staged below.
// The discovered set must likewise be identical across it (a localize that
// landed a directory discovery can see would change it and make a refresh
// owed).
func TestAssistantDependencyBotsLocalizeKeepsTheCatalogConsistentWithDiscovery(t *testing.T) {
	s, root, _ := assistantDependencyLocalizeFixture(t)
	ownerDir := filepath.Join(root, "bots", "router")
	if err := os.MkdirAll(ownerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "main.bot"), []byte("workflow router {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "manifest.yaml"), []byte("name: router\nschema_version: 1\ndescription: router bot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	static := "# catalog\n\n<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n\n<!-- ITERION:CATALOG:GENERATED:END -->\n"
	if err := os.WriteFile(filepath.Join(ownerDir, "iterion-bot-catalog-static.md"), []byte(static), 0o644); err != nil {
		t.Fatal(err)
	}
	// The consumer is armed as a catalog owner too: a refresh scoped to the
	// consumer bundle — the shape a localize-scoped refresh would take — has
	// its own catalogue to rewrite, and the assertions below hold it to the
	// same no-refresh invariant as the workspace owner.
	if err := os.WriteFile(filepath.Join(root, "bots", "planner", "iterion-bot-catalog-static.md"), []byte(static), 0o644); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(ownerDir, "skills", "iterion-bot-catalog.md")
	plannerCatalogPath := filepath.Join(root, "bots", "planner", "skills", "iterion-bot-catalog.md")
	if _, err := botregistry.RegenerateWhatsNextCatalog(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(catalogPath); err != nil {
		t.Fatalf("catalog owner wrote no generated catalog: %v", err)
	}

	// Make the on-disk catalogue stale on an unrelated detail: if ANY refresh
	// fired during the localize, the bytes would visibly change to carry it.
	manifestPath := filepath.Join(ownerDir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte("name: router\nschema_version: 1\ndescription: router bot, restyled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	plannerBefore, err := os.ReadFile(plannerCatalogPath)
	if err != nil {
		t.Fatalf("the consumer bundle owns no rendered catalogue to guard: %v", err)
	}
	discoveredBefore := discoveredPaths(t, root)
	if !strings.Contains(filepath.ToSlash(discoveredBefore), "shared-planner") {
		t.Fatalf("precondition: the pinned materialization must already be discovered:\n%s", discoveredBefore)
	}

	rec := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{
		EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source",
	})
	if rec.Code != 200 {
		t.Fatalf("localize = %d %s", rec.Code, rec.Body.String())
	}
	afterBytes, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatalf("the localize rewrote the catalogue while the workspace discovery was unchanged (a refresh fired for a set that did not move):\n--- before ---\n%s\n--- after ---\n%s", beforeBytes, afterBytes)
	}
	plannerAfter, err := os.ReadFile(plannerCatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(plannerBefore) != string(plannerAfter) {
		t.Fatalf("the localize rewrote the consumer bundle's own catalogue (a refresh fired scoped to the consumer):\n--- before ---\n%s\n--- after ---\n%s", plannerBefore, plannerAfter)
	}
	discoveredAfter := discoveredPaths(t, root)
	if discoveredBefore != discoveredAfter {
		t.Fatalf("the localize changed the discovered set, so the catalogue it leaves behind no longer matches discovery — a refresh is owed:\n--- before ---\n%s\n--- after ---\n%s", discoveredBefore, discoveredAfter)
	}
	localizedRel := filepath.ToSlash(filepath.Join("plugins", "shared-planner"))
	if strings.Contains(filepath.ToSlash(discoveredAfter), localizedRel) {
		t.Fatalf("the localized source must stay inside the consumer bundle's subtree, which discovery never walks while it claims the consumer bundle:\n%s", discoveredAfter)
	}

	// A second localize of the same dependency (hash unchanged) is refused as
	// already localized and must attempt no rewrite at all.
	second := assistantDependencyLocalizeCall(t, s, assistantDependencyBotsLocalizeRequest{
		EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "chore(planner): vendor shared planner source",
	})
	if second.Code != 409 || !strings.Contains(second.Body.String(), "already localized") {
		t.Fatalf("second localize = %d %s", second.Code, second.Body.String())
	}
	secondBytes, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(secondBytes) {
		t.Fatalf("the refused second localize rewrote the catalogue:\n--- before ---\n%s\n--- after ---\n%s", afterBytes, secondBytes)
	}
	plannerSecond, err := os.ReadFile(plannerCatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(plannerAfter) != string(plannerSecond) {
		t.Fatalf("the refused second localize rewrote the consumer bundle's own catalogue:\n--- before ---\n%s\n--- after ---\n%s", plannerAfter, plannerSecond)
	}
}

// discoveredPaths snapshots the bundle set the catalogue renders from: one
// line per discovered entry, name, workspace-relative path and enabled flag.
func discoveredPaths(t *testing.T, root string) string {
	t.Helper()
	entries, err := botregistry.List(botregistry.ListOptions{Paths: botregistry.DefaultPaths(root), Workdir: root})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		rel, err := filepath.Rel(root, e.Path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Name+" "+filepath.ToSlash(rel)+" enabled="+strconv.FormatBool(e.Enabled))
	}
	return strings.Join(out, "\n")
}
