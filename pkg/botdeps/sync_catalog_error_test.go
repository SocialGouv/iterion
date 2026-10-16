package botdeps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// breakCatalogOwner vendors a catalog owner under <workdir>/bots/nexie whose
// hand-authored template is a self-referencing symlink: ELOOP, which no
// privilege reads, so the regeneration the materializing install runs fails
// whoever runs the test. It returns the bundle root the error names.
func breakCatalogOwner(t *testing.T, workdir string) string {
	t.Helper()
	dir := filepath.Join(workdir, "bots", "nexie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: nexie\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow nexie {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(dir, "iterion-bot-catalog-static.md")
	if err := os.Symlink(tmpl, tmpl); err != nil {
		t.Fatalf("symlink unavailable: %v", err)
	}
	return dir
}

// lockOneLocalDependency writes a bots.lock pinning one local source bundle
// at its real content hash — the shape Sync materializes.
func lockOneLocalDependency(t *testing.T, workdir, name string) {
	t.Helper()
	source := filepath.Join(workdir, "source")
	writeUpdateTestBundle(t, source, "0.1.0", "workflow shared {}\n")
	hash, err := bundle.ContentHashDir(source)
	if err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("version: 1\ndependencies:\n  %s:\n    source: ./source\n    ref: local-test\n    bundle_sha256: %s\n", name, hash)
	if err := os.WriteFile(filepath.Join(workdir, botlock.FileName), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSync_ReportsTheCatalogRegenFailure: a dependency materializes and its
// hash verifies, so the sync succeeded — but the catalog the orchestrator
// routes on was not refreshed, and the result is every consumer's only word
// on it (the CLI prints it, the studio's dependency actions relay it).
func TestSync_ReportsTheCatalogRegenFailure(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	lockOneLocalDependency(t, workdir, "shared-planner")

	results, err := Sync(t.Context(), workdir)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(results) != 1 || !results[0].Changed {
		t.Fatalf("the materialization itself must still succeed: %+v", results)
	}
	wire, err := json.Marshal(results[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	msg, _ := got["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the sync result hides the catalog regeneration failure: %s", wire)
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
}

// TestSync_SilentWhenTheCatalogRefreshSucceeds: a workspace with no catalog
// owner has nothing to refresh, and a verified-unchanged dependency runs no
// install at all — neither adds a word to the result.
func TestSync_SilentWhenTheCatalogRefreshSucceeds(t *testing.T) {
	workdir := t.TempDir()
	lockOneLocalDependency(t, workdir, "shared-planner")

	first, err := Sync(t.Context(), workdir)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	second, err := Sync(t.Context(), workdir)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	for label, results := range map[string][]SyncResult{"first": first, "second": second} {
		wire, err := json.Marshal(results)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), "catalog_error") {
			t.Errorf("%s sync reports a catalog failure that did not happen: %s", label, wire)
		}
	}
}

// TestUpdate_ReportsTheCatalogRegenFailure: `iterion bots update` rewrites
// the pin and materializes it, so it refreshes the catalog too — and the
// UpdateResult is what its CLI and the studio host action print.
func TestUpdate_ReportsTheCatalogRegenFailure(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	lockOneLocalDependency(t, workdir, "shared-planner")
	if _, err := Sync(t.Context(), workdir); err != nil {
		t.Fatalf("initial Sync: %v", err)
	}
	// A new version of the source: the update has real content to
	// materialize, so it runs the install (and its catalog refresh).
	writeUpdateTestBundle(t, filepath.Join(workdir, "source"), "0.2.0", "workflow refreshed {}\n")

	res, err := Update(t.Context(), UpdateOptions{Workdir: workdir, Name: "shared-planner"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.InstalledPath == "" {
		t.Fatalf("the update itself must still succeed: %#v", res)
	}
	if res.CatalogError == "" {
		t.Fatalf("the update result hides the catalog regeneration failure: %#v", res)
	}
	if !strings.Contains(res.CatalogError, owner) {
		t.Errorf("CatalogError %q does not name the catalog owner it could not refresh (%s)", res.CatalogError, owner)
	}
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if msg, _ := got["catalog_error"].(string); msg != res.CatalogError {
		t.Errorf("the wire key catalog_error = %q, want the reported cause %q", msg, res.CatalogError)
	}
}

// writeNamedTestBundle writes a minimal bundle whose manifest name matches
// the lock entry — what an install verifies before it lands.
func writeNamedTestBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"),
		[]byte("name: "+name+"\nversion: 0.1.0\nschema_version: 1\nenabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow "+name+" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSync_AnAbortReturnsWhatItMaterialized: the sync aborts on a
// dependency whose source cannot match its pin, but the one before it IS
// installed and its catalog refresh failed. Dropping those results with the
// error loses the only report of a stale routing table: the next sync finds
// that dependency at its pin and installs nothing, forever.
func TestSync_AnAbortReturnsWhatItMaterialized(t *testing.T) {
	workdir := t.TempDir()
	owner := breakCatalogOwner(t, workdir)
	good := filepath.Join(workdir, "source-alpha")
	writeNamedTestBundle(t, good, "alpha")
	hash, err := bundle.ContentHashDir(good)
	if err != nil {
		t.Fatal(err)
	}
	writeNamedTestBundle(t, filepath.Join(workdir, "source-zbeta"), "zbeta")
	lock := fmt.Sprintf("version: 1\ndependencies:\n"+
		"  alpha:\n    source: ./source-alpha\n    ref: local-test\n    bundle_sha256: %s\n"+
		"  zbeta:\n    source: ./source-zbeta\n    ref: local-test\n    bundle_sha256: %s\n",
		hash, strings.Repeat("0", 64))
	if err := os.WriteFile(filepath.Join(workdir, botlock.FileName), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Sync(t.Context(), workdir)

	if err == nil {
		t.Fatalf("test premise: the sync must abort on zbeta; results=%#v", results)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, ".botz", "alpha", "main.bot")); statErr != nil {
		t.Fatalf("test premise: alpha must have materialized before the abort: %v", statErr)
	}
	if len(results) != 1 || results[0].Name != "alpha" {
		t.Fatalf("the abort dropped the dependency it had already materialized: %#v", results)
	}
	if results[0].CatalogError == "" {
		t.Fatalf("the returned result hides the catalog refresh that failed: %#v", results[0])
	}
	if !strings.Contains(results[0].CatalogError, owner) {
		t.Errorf("CatalogError %q does not name the catalog owner it could not refresh (%s)", results[0].CatalogError, owner)
	}
}

// TestStageOne_PrivateStagingNeitherRefreshesNorReports: private staging
// preserves the pinned catalog bytes by skipping the regeneration, so even
// an unreadable catalog owner costs it nothing — and it has no failure to
// report. A warning here would send the operator after a refresh that was
// never attempted.
func TestStageOne_PrivateStagingNeitherRefreshesNorReports(t *testing.T) {
	workdir := t.TempDir()
	breakCatalogOwner(t, workdir)
	lockOneLocalDependency(t, workdir, "shared-planner")
	lock, err := botlock.Load(workdir)
	if err != nil {
		t.Fatal(err)
	}

	staged, err := StageOne(t.Context(), workdir, t.TempDir(), "shared-planner", lock.Dependencies["shared-planner"])
	if err != nil {
		t.Fatalf("StageOne: %v", err)
	}
	if staged.InstalledPath == "" {
		t.Fatalf("the staging itself must still succeed: %#v", staged)
	}
	if staged.CatalogError != "" {
		t.Errorf("private staging reports a catalog failure it never attempted: %q", staged.CatalogError)
	}
}
