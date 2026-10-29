package botinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

const producerFragmentBody = "schema shared_state {}\n"

// TestInstallAloneRefusesABundleWithSharedFragments is ARB-81's refusal: one
// install cannot resolve a dependency whose producer the project lockfile
// materializes, and resolving on the spot would make the verdict depend on
// the alphabetical order Sync happens to walk. The refusal names the command
// that does it in the right order, and nothing is written.
func TestInstallAloneRefusesABundleWithSharedFragments(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "src")
	writeConsumerBundle(t, source, "a-bot", "z-lib")

	_, err := Install(t.Context(), Options{Source: source, Workdir: workdir, SkipCatalog: true})
	if err == nil {
		t.Fatal("Install resolved shared fragments on its own")
	}
	for _, want := range []string{`bundle "a-bot" declares dependencies.fragments`, "shared (from z-lib)", "iterion bots sync"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(workdir, ".botz", "a-bot")); !os.IsNotExist(statErr) {
		t.Fatalf("the refusal wrote an install anyway: %v", statErr)
	}
}

// TestInstallRefusesBeforeRemovingAnExistingTarget pins the refusal's
// PLACE: --force removes the target whole before copying, so a refusal
// raised after that point would destroy an install to report an error.
func TestInstallRefusesBeforeRemovingAnExistingTarget(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "src")
	writeConsumerBundle(t, source, "a-bot", "z-lib")
	existing := filepath.Join(workdir, ".botz", "a-bot")
	writeInstallTestFile(t, filepath.Join(existing, "main.bot"), "workflow kept {}\n")

	if _, err := Install(t.Context(), Options{Source: source, Workdir: workdir, SkipCatalog: true, Force: true, ReplaceUnrecorded: true}); err == nil {
		t.Fatal("Install resolved shared fragments on its own")
	}
	body, err := os.ReadFile(filepath.Join(existing, "main.bot"))
	if err != nil || string(body) != "workflow kept {}\n" {
		t.Fatalf("the refusal destroyed the existing install: %q %v", body, err)
	}
}

// TestDeferredFragmentsLetTheCallerOwnTheOrder is the other half of the
// refusal: botdeps sets DeferFragments because it materializes the whole
// lockfile first, so the install itself must go through.
func TestDeferredFragmentsLetTheCallerOwnTheOrder(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "src")
	writeConsumerBundle(t, source, "a-bot", "z-lib")

	res, err := Install(t.Context(), Options{Source: source, Workdir: workdir, SkipCatalog: true, DeferFragments: true})
	if err != nil {
		t.Fatalf("Install with DeferFragments: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(res.InstalledPath, "main.bot")); statErr != nil {
		t.Fatalf("the bundle was not installed: %v", statErr)
	}
}

// TestResolveFragmentsRefusesToShadowAShippedFragment holds ADR-145's
// no-shadow clause: a silent overwrite would make `import "lib/x.bot"` mean
// one thing in the repository and another after an install, which is the
// drift the record exists to end.
func TestResolveFragmentsRefusesToShadowAShippedFragment(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeConsumerBundle(t, consumer, "a-bot", "z-lib")
	shipped := "schema shared_state { its_own }\n"
	writeInstallTestFile(t, filepath.Join(consumer, "lib", "shared.bot"), shipped)

	_, err := ResolveFragments(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveFragments overwrote a fragment the bundle ships")
	}
	if !strings.Contains(err.Error(), "would shadow lib/shared.bot, which this bundle ships") {
		t.Fatalf("error = %q", err)
	}
	body, readErr := os.ReadFile(filepath.Join(consumer, "lib", "shared.bot"))
	if readErr != nil || string(body) != shipped {
		t.Fatalf("the shipped fragment was overwritten: %q %v", body, readErr)
	}
}

// TestResolveFragmentsNamesEveryLayerOnAMiss keeps the typed miss honest:
// the dependency exists at install, so the install is where the miss is
// named — with the bundle, the fragment and every directory looked in.
func TestResolveFragmentsNamesEveryLayerOnAMiss(t *testing.T) {
	workdir := t.TempDir()
	consumer := filepath.Join(workdir, "stage", "a-bot")
	writeConsumerBundle(t, consumer, "a-bot", "z-lib")

	_, err := ResolveFragments(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveFragments succeeded with no producer anywhere")
	}
	for _, want := range []string{
		`shared fragment "shared" comes from bundle "z-lib", which is not materialized`,
		filepath.Join(workdir, "stage", "z-lib"),
		filepath.Join(workdir, ".botz", "z-lib"),
		"iterion bots sync",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestResolveFragmentsFallsBackToTheWorkspaceLayer proves the second layer
// is real: a consumer staged into a private destination still resolves
// against the workspace's own .botz, which is what StageOne promises.
func TestResolveFragmentsFallsBackToTheWorkspaceLayer(t *testing.T) {
	workdir := t.TempDir()
	writeProducerBundle(t, filepath.Join(workdir, ".botz", "z-lib"), "z-lib")
	consumer := filepath.Join(workdir, "stage", "a-bot")
	writeConsumerBundle(t, consumer, "a-bot", "z-lib")

	resolved, err := ResolveFragments(workdir, consumer)
	if err != nil {
		t.Fatalf("ResolveFragments: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Path != "lib/shared.bot" {
		t.Fatalf("resolved = %+v, want one lib/shared.bot", resolved)
	}
	body, readErr := os.ReadFile(filepath.Join(consumer, "lib", "shared.bot"))
	if readErr != nil || string(body) != producerFragmentBody {
		t.Fatalf("materialized bytes = %q %v, want the producer's", body, readErr)
	}
	note, noteErr := bundle.ReadMaterializedFragments(consumer)
	if noteErr != nil || len(note) != 1 || note[0].Path != "lib/shared.bot" || note[0].SHA256 != bundle.SHA256Bytes([]byte(producerFragmentBody)) {
		t.Fatalf("note = %+v, err=%v; want one lib/shared.bot entry at the producer's digest", note, noteErr)
	}
}

// TestResolveFragmentsRefusesAProducerUnderTheWrongName keeps the
// allow-list exact, as the workflow contract already does: a directory named
// like the dependency whose manifest says otherwise is not that bundle.
func TestResolveFragmentsRefusesAProducerUnderTheWrongName(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeProducerBundle(t, filepath.Join(dest, "z-lib"), "someone-else")
	consumer := filepath.Join(dest, "a-bot")
	writeConsumerBundle(t, consumer, "a-bot", "z-lib")

	_, err := ResolveFragments(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveFragments accepted a producer whose manifest name differs")
	}
	if !strings.Contains(err.Error(), `declares manifest name "someone-else", not "z-lib"`) {
		t.Fatalf("error = %q", err)
	}
}

// TestResolveFragmentsIsIdempotent re-runs the reconciliation over its own
// output: a resolver that refused its own previous copy as a shadow would
// break every second sync.
func TestResolveFragmentsIsIdempotent(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeConsumerBundle(t, consumer, "a-bot", "z-lib")

	first, err := ResolveFragments(workdir, consumer)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := ResolveFragments(workdir, consumer)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if len(second) != len(first) || second[0] != first[0] {
		t.Fatalf("second resolve = %+v, first = %+v", second, first)
	}
}

// TestResolveFragmentsRefusesToDeleteAnEditedFragment keeps the removal of a
// dropped dependency to what this installer provably wrote: a file edited
// after it was materialized is the operator's, and is named rather than
// destroyed.
func TestResolveFragmentsRefusesToDeleteAnEditedFragment(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeConsumerBundle(t, consumer, "a-bot", "z-lib")
	if _, err := ResolveFragments(workdir, consumer); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	edited := "schema shared_state { edited_by_hand }\n"
	writeInstallTestFile(t, filepath.Join(consumer, "lib", "shared.bot"), edited)
	// The dependency is dropped from the manifest.
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\n")

	_, err := ResolveFragments(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveFragments deleted a fragment edited after it was materialized")
	}
	if !strings.Contains(err.Error(), "was edited after it was materialized and is no longer declared") {
		t.Fatalf("error = %q", err)
	}
	body, readErr := os.ReadFile(filepath.Join(consumer, "lib", "shared.bot"))
	if readErr != nil || string(body) != edited {
		t.Fatalf("the edited fragment was destroyed: %q %v", body, readErr)
	}
}

func writeProducerBundle(t *testing.T, dir, name string) {
	t.Helper()
	writeInstallTestFile(t, filepath.Join(dir, "main.bot"), "workflow producer {}\n")
	writeInstallTestFile(t, filepath.Join(dir, "lib", "shared.bot"), producerFragmentBody)
	writeInstallTestFile(t, filepath.Join(dir, "manifest.yaml"), "name: "+name+"\nschema_version: 1\nenabled: false\nexports:\n  fragments:\n    - id: shared\n      path: lib/shared.bot\n")
}

func writeConsumerBundle(t *testing.T, dir, name, from string) {
	t.Helper()
	writeInstallTestFile(t, filepath.Join(dir, "main.bot"), "import \"lib/shared.bot\"\n\nworkflow consumer {}\n")
	writeInstallTestFile(t, filepath.Join(dir, "manifest.yaml"), "name: "+name+"\nschema_version: 1\nenabled: false\ndependencies:\n  fragments:\n    - name: shared\n      from: "+from+"\n")
}

func writeInstallTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestADroppedDependencyStopsAnsweringTheImport is the removal path's own
// witness, exercised WITHOUT a re-install: a leftover copy would keep
// answering `import "lib/shared.bot"` with a fragment the manifest no longer
// declares, which is the silent fallback the refusals exist to prevent.
func TestADroppedDependencyStopsAnsweringTheImport(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeConsumerBundle(t, consumer, "a-bot", "z-lib")
	if _, err := ResolveFragments(workdir, consumer); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	materialized := filepath.Join(consumer, "lib", "shared.bot")
	if _, err := os.Stat(materialized); err != nil {
		t.Fatalf("the first resolve materialized nothing: %v", err)
	}

	// The manifest drops the dependency; the materialized bytes are left
	// exactly as the resolver wrote them.
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\n")
	resolved, err := ResolveFragments(workdir, consumer)
	if err != nil {
		t.Fatalf("resolve after dropping the dependency: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved = %+v, want none", resolved)
	}
	if _, err := os.Stat(materialized); !os.IsNotExist(err) {
		t.Fatalf("a dropped dependency still answers the import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(consumer, bundle.MaterializedFragmentsFile)); !os.IsNotExist(err) {
		t.Fatalf("the note outlived the dependency it recorded: %v", err)
	}
}

// TestResolveFragmentsLeavesABundleWithNothingDeclaredAlone keeps the pass
// off files it has no business touching: a bundle that declares no
// dependency and recorded none owns every file it has, so nothing is
// written and nothing is removed.
func TestResolveFragmentsLeavesABundleWithNothingDeclaredAlone(t *testing.T) {
	workdir := t.TempDir()
	consumer := filepath.Join(workdir, ".botz", "plain")
	writeInstallTestFile(t, filepath.Join(consumer, "main.bot"), "workflow plain {}\n")
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: plain\nschema_version: 1\nenabled: false\n")
	writeInstallTestFile(t, filepath.Join(consumer, "lib", "its-own.bot"), "schema its_own {}\n")

	resolved, err := ResolveFragments(workdir, consumer)
	if err != nil {
		t.Fatalf("ResolveFragments: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved = %+v, want none", resolved)
	}
	body, readErr := os.ReadFile(filepath.Join(consumer, "lib", "its-own.bot"))
	if readErr != nil || string(body) != "schema its_own {}\n" {
		t.Fatalf("the pass touched a fragment the bundle ships: %q %v", body, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(consumer, bundle.MaterializedFragmentsFile)); !os.IsNotExist(statErr) {
		t.Fatalf("the pass wrote a note for a bundle that declares nothing: %v", statErr)
	}
}

// TestAFileUnderTheReservedNoteNameIsNamed refuses the silent
// reinterpretation: the note's name is reserved local state, and a bundle
// that ships something else there is told so by name and line rather than
// having its content quietly read as a record — a hash computed over a
// record nobody could read would be a verdict with no basis.
func TestAFileUnderTheReservedNoteNameIsNamed(t *testing.T) {
	workdir := t.TempDir()
	consumer := filepath.Join(workdir, ".botz", "plain")
	writeInstallTestFile(t, filepath.Join(consumer, "main.bot"), "workflow plain {}\n")
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: plain\nschema_version: 1\nenabled: false\n")
	writeInstallTestFile(t, filepath.Join(consumer, bundle.MaterializedFragmentsFile), "shipped by the producer\n")

	_, err := ResolveFragments(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveFragments read an arbitrary file as its own record")
	}
	for _, want := range []string{bundle.MaterializedFragmentsFile, "line 1", "shipped by the producer"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}
