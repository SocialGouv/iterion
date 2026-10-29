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

const sharedFragmentBody = "schema shared_state {}\n"

// TestSyncResolvesASharedFragmentInBothOrders is the witness ARB-81 asks
// for. Sync walks `bots.lock` alphabetically (sort.Strings), so a consumer
// whose name sorts BEFORE its producer is installed while the producer's
// `.botz/` directory does not exist yet. A resolution inside the first pass
// would refuse that correct configuration and accept the mirror one: the
// verdict would be an accident of sorting. Both orders are exercised, so a
// test that can only redden in one of them is flaky, not a witness.
func TestSyncResolvesASharedFragmentInBothOrders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		consumer string
		producer string
	}{
		{"consumer sorts first", "a-bot", "z-lib"},
		{"producer sorts first", "z-bot", "a-lib"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumer, producer := tc.consumer, tc.producer
			if (consumer < producer) != (tc.name == "consumer sorts first") {
				t.Fatalf("the case %q does not set up the order it names: %q vs %q", tc.name, consumer, producer)
			}
			workdir := writeFragmentSharingWorkspace(t, consumer, producer)

			results, err := Sync(t.Context(), workdir)
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if len(results) != 2 {
				t.Fatalf("Sync returned %d results, want 2", len(results))
			}
			var consumerResult SyncResult
			for _, r := range results {
				if r.Name == consumer {
					consumerResult = r
				}
			}
			if consumerResult.Fragments != 1 {
				t.Fatalf("consumer %q reported %d resolved fragments, want 1", consumer, consumerResult.Fragments)
			}
			materialized := filepath.Join(workdir, ".botz", consumer, "lib", "shared.bot")
			got, readErr := os.ReadFile(materialized)
			if readErr != nil {
				t.Fatalf("the fragment %q imports is not materialized: %v", consumer, readErr)
			}
			if string(got) != sharedFragmentBody {
				t.Fatalf("materialized bytes = %q, want the producer's %q", got, sharedFragmentBody)
			}
			note, noteErr := bundle.ReadMaterializedFragments(filepath.Join(workdir, ".botz", consumer))
			if noteErr != nil || len(note) != 1 || note[0].Path != "lib/shared.bot" {
				t.Fatalf("note = %+v, err=%v; want one lib/shared.bot entry", note, noteErr)
			}
			if note[0].SHA256 != bundle.SHA256Bytes([]byte(sharedFragmentBody)) {
				t.Fatalf("note digest = %q, want the materialized bytes' digest", note[0].SHA256)
			}
		})
	}
}

// TestASecondSyncLeavesTheConsumerAtItsPin proves the reconciliation is
// idempotent: resolving a dependency must not move the hash `bots.lock`
// pins, or the second sync would re-install forever and every pin check
// would read drift.
func TestASecondSyncLeavesTheConsumerAtItsPin(t *testing.T) {
	workdir := writeFragmentSharingWorkspace(t, "a-bot", "z-lib")
	if _, err := Sync(t.Context(), workdir); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	lock, err := botlock.Load(workdir)
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(workdir, ".botz", "a-bot")
	hash, err := bundle.ContentHashDir(installed)
	if err != nil {
		t.Fatal(err)
	}
	if want := lock.Dependencies["a-bot"].BundleSHA256; hash != want {
		t.Fatalf("after resolution the consumer hashes %s, lock pins %s: an idempotent sync reads as drift", hash, want)
	}
	second, err := Sync(t.Context(), workdir)
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	for _, r := range second {
		if r.Name == "a-bot" && r.Changed {
			t.Fatal("the second sync re-installed a consumer already at its pin")
		}
	}
	if _, err := os.ReadFile(filepath.Join(installed, "lib", "shared.bot")); err != nil {
		t.Fatalf("the second sync lost the materialized fragment: %v", err)
	}
}

// TestADroppedFragmentDependencyStopsAnsweringTheImport refuses the silent
// fallback a leftover copy would be: an import that still resolves to a
// fragment the manifest no longer declares is a stale answer nobody asked
// for.
func TestADroppedFragmentDependencyStopsAnsweringTheImport(t *testing.T) {
	workdir := writeFragmentSharingWorkspace(t, "a-bot", "z-lib")
	if _, err := Sync(t.Context(), workdir); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	// The consumer drops the dependency; its source is re-pinned, as an
	// operator editing a manifest would re-pin it.
	consumerSrc := filepath.Join(workdir, "src-a-bot")
	writeFragmentTestFile(t, filepath.Join(consumerSrc, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\n")
	repinFragmentLock(t, workdir, map[string]string{"a-bot": consumerSrc, "z-lib": filepath.Join(workdir, "src-z-lib")})

	if _, err := Sync(t.Context(), workdir); err != nil {
		t.Fatalf("Sync after dropping the dependency: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "a-bot", "lib", "shared.bot")); !os.IsNotExist(err) {
		t.Fatalf("a dropped dependency still answers the import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "a-bot", bundle.MaterializedFragmentsFile)); !os.IsNotExist(err) {
		t.Fatalf("the note outlived the dependency it recorded: %v", err)
	}
}

// TestSyncNamesAnUnexportedFragment keeps the miss typed where the
// dependency exists — at the install, naming the bundle and what it does
// export.
func TestSyncNamesAnUnexportedFragment(t *testing.T) {
	workdir := writeFragmentSharingWorkspace(t, "a-bot", "z-lib")
	producerSrc := filepath.Join(workdir, "src-z-lib")
	writeFragmentTestFile(t, filepath.Join(producerSrc, "manifest.yaml"), "name: z-lib\nschema_version: 1\nenabled: false\nexports:\n  fragments:\n    - id: other\n      path: lib/shared.bot\n")
	repinFragmentLock(t, workdir, map[string]string{"a-bot": filepath.Join(workdir, "src-a-bot"), "z-lib": producerSrc})

	_, err := Sync(t.Context(), workdir)
	if err == nil {
		t.Fatal("Sync resolved a fragment the producer does not export")
	}
	for _, want := range []string{`bundle "z-lib"`, `does not export fragment "shared"`, "it exports: other"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

// writeFragmentSharingWorkspace lays out a producer that exports one
// fragment and a consumer that declares it, both pinned in bots.lock under
// the given names so a caller chooses which one sorts first.
func writeFragmentSharingWorkspace(t *testing.T, consumer, producer string) string {
	t.Helper()
	workdir := t.TempDir()
	producerSrc := filepath.Join(workdir, "src-"+producer)
	writeFragmentTestFile(t, filepath.Join(producerSrc, "main.bot"), "workflow "+strings.ReplaceAll(producer, "-", "_")+" {}\n")
	writeFragmentTestFile(t, filepath.Join(producerSrc, "lib", "shared.bot"), sharedFragmentBody)
	writeFragmentTestFile(t, filepath.Join(producerSrc, "manifest.yaml"), fmt.Sprintf("name: %s\nschema_version: 1\nenabled: false\nexports:\n  fragments:\n    - id: shared\n      path: lib/shared.bot\n", producer))

	consumerSrc := filepath.Join(workdir, "src-"+consumer)
	writeFragmentTestFile(t, filepath.Join(consumerSrc, "main.bot"), "import \"lib/shared.bot\"\n\nworkflow "+strings.ReplaceAll(consumer, "-", "_")+" {}\n")
	writeFragmentTestFile(t, filepath.Join(consumerSrc, "manifest.yaml"), fmt.Sprintf("name: %s\nschema_version: 1\nenabled: false\ndependencies:\n  fragments:\n    - name: shared\n      from: %s\n", consumer, producer))

	repinFragmentLock(t, workdir, map[string]string{consumer: consumerSrc, producer: producerSrc})
	return workdir
}

func repinFragmentLock(t *testing.T, workdir string, sources map[string]string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("version: 1\ndependencies:\n")
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	// Written in any order: botlock decodes a map, and Sync is what sorts.
	for _, name := range names {
		hash, err := bundle.ContentHashDir(sources[name])
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "  %s:\n    source: ./%s\n    ref: local-test\n    bundle_sha256: %s\n", name, filepath.Base(sources[name]), hash)
	}
	writeFragmentTestFile(t, filepath.Join(workdir, botlock.FileName), b.String())
}

func writeFragmentTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
