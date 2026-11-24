package botdeps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// catalogOwnerTemplate is the hand-authored source a router bundle ships:
// editorial text around the generated markers the regeneration splices
// between.
const catalogOwnerTemplate = "# Router catalogue\n\n" +
	"<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n" +
	"frozen by the packer\n" +
	"<!-- ITERION:CATALOG:GENERATED:END -->\n\n" +
	"## The routing ritual\n"

// writeCatalogOwnerBundle writes a router bundle the way its PRODUCER ships
// one: the hand-authored template at the bundle root, and the generated
// catalogue the packer froze under skills/. This repository commits that
// generated file for whats-next and issue-triage, so a fetched producer
// carries it and the pin in bots.lock covers its bytes.
func writeCatalogOwnerBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, bundle.DirSkills), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"manifest.yaml":                 "name: " + name + "\nversion: 0.1.0\nschema_version: 1\n",
		"main.bot":                      "workflow " + name + " {}\n",
		"iterion-bot-catalog-static.md": catalogOwnerTemplate,
		"skills/iterion-bot-catalog.md": catalogOwnerTemplate,
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSync_AMaterializedCatalogOwnerStaysAtItsPin: a router bot vendored as
// a dependency is a catalog OWNER like any other, so the regeneration that
// runs with every install rewrites its skills/iterion-bot-catalog.md with
// the CONSUMER workspace's routing table — which is the feature (the
// vendored router routes to the bots that are actually there).
//
// That write must not move the bundle's identity: bundle.ContentHashDir is
// the verdict every pin check reads, so a moved hash makes bots.lock false,
// fails the post-install verification, and re-materializes the dependency at
// every sync (ARB-109 = B).
func TestSync_AMaterializedCatalogOwnerStaysAtItsPin(t *testing.T) {
	workdir := t.TempDir()
	// A workspace bot the vendored router must route to: its row is what
	// makes the regenerated table differ from the one the packer froze. It
	// has to be ENABLED — a disabled bot carries no row.
	writeEnabledTestBundle(t, filepath.Join(workdir, "bots", "localbot"), "localbot")
	source := filepath.Join(workdir, "source-router")
	writeCatalogOwnerBundle(t, source, "routerbot")
	pin, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("version: 1\ndependencies:\n  routerbot:\n    source: ./source-router\n    ref: local-test\n    bundle_sha256: %s\n", pin)
	if err := os.WriteFile(filepath.Join(workdir, botlock.FileName), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := Sync(t.Context(), workdir)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(first) != 1 || !first[0].Changed {
		t.Fatalf("the dependency must materialize: %+v", first)
	}
	if first[0].CatalogError != "" {
		t.Fatalf("the catalog refresh failed: %s", first[0].CatalogError)
	}
	installed := filepath.Join(workdir, ".botz", "routerbot")

	// The run reads the live table: the file on disk carries the consumer
	// workspace's bots, not the set the packer froze.
	live, err := os.ReadFile(filepath.Join(installed, bundle.DirSkills, "iterion-bot-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), "localbot") {
		t.Errorf("the vendored router's catalog does not carry the workspace's bot, so the regeneration never reached it:\n%s", live)
	}

	// The pin stays true: the identity of the install is the content its
	// producer shipped.
	hash, err := bundle.ContentHashDir(installed)
	if err != nil {
		t.Fatal(err)
	}
	if hash != pin {
		t.Errorf("ContentHashDir(%s) = %s after the catalog regeneration, want the pinned %s: bots.lock reads the install as drifted", installed, hash, pin)
	}

	// And no sync re-materializes it: the hash short-circuit holds.
	second, err := Sync(t.Context(), workdir)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if len(second) != 1 || second[0].Changed {
		t.Errorf("the second sync re-materialized a dependency that is at its pin: %+v", second)
	}
}

// writeEnabledTestBundle writes a minimal ENABLED bundle: the regenerated
// catalogue carries a row per enabled bot, so a disabled one proves nothing
// about a table that reached the file.
func writeEnabledTestBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"),
		[]byte("name: "+name+"\nversion: 0.1.0\nschema_version: 1\nenabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow "+name+" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
