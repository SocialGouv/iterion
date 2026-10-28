package botregistry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The walk claims what bundle.DirClaim claims (ADR-140): a manifest.yml
// consumer is a bundle under its own name; a claimed consumer without
// main.bot is a loud skipped source whose subtree stays private; a foreign
// manifest beside a main.bot claims nothing and the walk descends.

func writeWalkFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listedPaths(t *testing.T, dir string) string {
	t.Helper()
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		rel, _ := filepath.Rel(dir, e.Path)
		paths = append(paths, filepath.ToSlash(rel))
	}
	sort.Strings(paths)
	return strings.Join(paths, " ")
}

func TestListClaimsAManifestYmlConsumerUnderItsOwnName(t *testing.T) {
	dir := t.TempDir()
	consumer := filepath.Join(dir, "bots", "planner")
	writeWalkFile(t, filepath.Join(consumer, "main.bot"), "workflow planner {}\n")
	writeWalkFile(t, filepath.Join(consumer, "manifest.yml"), "name: planner\nschema_version: 1\n")
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || filepath.Base(entries[0].Path) != "planner" {
		var got []string
		for _, e := range entries {
			got = append(got, e.Path)
		}
		t.Fatalf("entries = %v: a manifest.yml consumer is claimed as the bundle planner, not a loose main", got)
	}
}

func TestListSkipsALoudAnUnopenableConsumerAndItsSubtree(t *testing.T) {
	dir := t.TempDir()
	consumer := filepath.Join(dir, "bots", "planner")
	writeWalkFile(t, filepath.Join(consumer, "manifest.yaml"), "name: planner\nschema_version: 1\n")
	writeWalkFile(t, filepath.Join(consumer, "plugins", "dep", "main.bot"), "workflow dep {}\n")
	writeWalkFile(t, filepath.Join(dir, "bots", "standalone.bot"), "workflow s {}\n")
	if got := listedPaths(t, dir); got != "bots/standalone.bot" {
		t.Fatalf("listed %q: the unopenable consumer's subtree stays private — the dep must not surface", got)
	}
	_, diags, err := ListWithDiagnostics(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	loud := false
	for _, d := range diags {
		if d.Kind == DiscoveryErrorUnopenable && strings.Contains(filepath.ToSlash(d.Path), "planner") {
			loud = true
		}
	}
	if !loud {
		t.Fatalf("diagnostics = %+v: the skipped consumer must be named, not dropped in silence", diags)
	}
}

func TestListDescendsAForeignManifestConsumer(t *testing.T) {
	dir := t.TempDir()
	consumer := filepath.Join(dir, "bots", "planner")
	writeWalkFile(t, filepath.Join(consumer, "main.bot"), "workflow planner {}\n")
	writeWalkFile(t, filepath.Join(consumer, "manifest.yaml"), "services:\n  web:\n    image: nginx\n")
	if got := listedPaths(t, dir); got != "bots/planner/main.bot" {
		t.Fatalf("listed %q: another tool's manifest claims nothing — the main.bot reads as the loose bot it is", got)
	}
}
