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

const sharedSkillBody = "---\nname: shared-playbook\ndescription: The gate the consumers used to each carry a copy of.\n---\n\nRun the gate, then report what it said.\n"

// TestSyncResolvesASharedSkillInBothOrders is ADR-144 clause 2's witness at
// the level that matters: a consumer that ships NO copy of the skill its
// `skills:` list names ends up with the producer's bytes where a shipped copy
// would have been. Both lock orders are exercised — Sync walks `bots.lock`
// alphabetically, so a consumer that sorts before its producer is installed
// while the producer's `.botz/` directory does not exist yet, and a witness
// that can only redden in one order is flaky rather than a witness.
func TestSyncResolvesASharedSkillInBothOrders(t *testing.T) {
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
			workdir := writeSkillSharingWorkspace(t, consumer, producer)

			results, err := Sync(t.Context(), workdir)
			if err != nil {
				t.Fatalf("Sync: %v", err)
			}
			var consumerResult SyncResult
			for _, r := range results {
				if r.Name == consumer {
					consumerResult = r
				}
			}
			if consumerResult.Skills != 1 {
				t.Fatalf("consumer %q reported %d resolved skills, want 1", consumer, consumerResult.Skills)
			}
			installed := filepath.Join(workdir, ".botz", consumer)
			got, readErr := os.ReadFile(filepath.Join(installed, "skills", "shared-playbook.md"))
			if readErr != nil {
				t.Fatalf("the skill %q references is not materialized: %v", consumer, readErr)
			}
			if string(got) != sharedSkillBody {
				t.Fatalf("materialized bytes = %q, want the producer's %q", got, sharedSkillBody)
			}
			// The effect, not the screen: the bot is only as smart as the
			// files in its skills directory.
			entries, dirErr := os.ReadDir(filepath.Join(installed, "skills"))
			if dirErr != nil || len(entries) != 1 {
				t.Fatalf("installed skills dir = %v (err %v), want exactly the resolved document", entries, dirErr)
			}
			record, recErr := bundle.ReadLocalStateOfKind(installed, bundle.LocalStateSkill)
			if recErr != nil || len(record) != 1 || record[0].Path != "skills/shared-playbook.md" {
				t.Fatalf("record = %+v, err=%v; want one skills/shared-playbook.md entry", record, recErr)
			}
			if record[0].SHA256 != bundle.SHA256Bytes([]byte(sharedSkillBody)) {
				t.Fatalf("record digest = %q, want the materialized bytes' digest", record[0].SHA256)
			}
			if record[0].KeptSHA256 != "" {
				t.Fatalf("record keeps producer bytes for a path the producer never shipped: %+v", record[0])
			}
		})
	}
}

// TestASecondSyncLeavesTheSkillConsumerAtItsPin is the reason the resolution
// is recorded at all: materializing a declared skill must not move the hash
// `bots.lock` pins, or the post-install verification refuses the install
// outright and every later sync re-installs forever.
func TestASecondSyncLeavesTheSkillConsumerAtItsPin(t *testing.T) {
	workdir := writeSkillSharingWorkspace(t, "a-bot", "z-lib")
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
	if _, err := os.ReadFile(filepath.Join(installed, "skills", "shared-playbook.md")); err != nil {
		t.Fatalf("the second sync lost the materialized skill: %v", err)
	}
}

// TestADroppedSkillDependencyStopsAnsweringTheReference refuses the stale
// answer a leftover copy would be: a `skills:` reference that still resolves
// to a document the manifest no longer declares is bundled knowledge nobody
// pinned.
func TestADroppedSkillDependencyStopsAnsweringTheReference(t *testing.T) {
	workdir := writeSkillSharingWorkspace(t, "a-bot", "z-lib")
	if _, err := Sync(t.Context(), workdir); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	consumerSrc := filepath.Join(workdir, "src-a-bot")
	writeFragmentTestFile(t, filepath.Join(consumerSrc, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\n")
	repinFragmentLock(t, workdir, map[string]string{"a-bot": consumerSrc, "z-lib": filepath.Join(workdir, "src-z-lib")})

	if _, err := Sync(t.Context(), workdir); err != nil {
		t.Fatalf("Sync after dropping the dependency: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "a-bot", "skills", "shared-playbook.md")); !os.IsNotExist(err) {
		t.Fatalf("a dropped dependency still answers the reference: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "a-bot", bundle.LocalStateFile)); !os.IsNotExist(err) {
		t.Fatalf("the record outlived the dependency it described: %v", err)
	}
}

// TestSyncNamesAnUnexportedSkill keeps the miss typed where the dependency
// exists — at the install, naming the bundle and what it does export —
// instead of leaving a skills directory one file short while the install
// reports success (ADR-144 clause 4).
func TestSyncNamesAnUnexportedSkill(t *testing.T) {
	workdir := writeSkillSharingWorkspace(t, "a-bot", "z-lib")
	producerSrc := filepath.Join(workdir, "src-z-lib")
	writeFragmentTestFile(t, filepath.Join(producerSrc, "manifest.yaml"), "name: z-lib\nschema_version: 1\nenabled: false\nexports:\n  skills:\n    - id: other\n      path: skills/shared-playbook.md\n")
	repinFragmentLock(t, workdir, map[string]string{"a-bot": filepath.Join(workdir, "src-a-bot"), "z-lib": producerSrc})

	_, err := Sync(t.Context(), workdir)
	if err == nil {
		t.Fatal("Sync resolved a skill the producer does not export")
	}
	for _, want := range []string{`bundle "z-lib"`, `does not export skill "shared-playbook"`, "it exports: other"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(workdir, ".botz", "a-bot", "skills", "shared-playbook.md")); !os.IsNotExist(statErr) {
		t.Fatalf("the refused resolution left a document behind: %v", statErr)
	}
}

// writeSkillSharingWorkspace lays out a producer that exports one skill
// document and a consumer that declares it and ships NO copy, both pinned in
// bots.lock under the given names so a caller chooses which one sorts first.
func writeSkillSharingWorkspace(t *testing.T, consumer, producer string) string {
	t.Helper()
	workdir := t.TempDir()
	producerSrc := filepath.Join(workdir, "src-"+producer)
	writeFragmentTestFile(t, filepath.Join(producerSrc, "main.bot"), "workflow "+strings.ReplaceAll(producer, "-", "_")+" {}\n")
	writeFragmentTestFile(t, filepath.Join(producerSrc, "skills", "shared-playbook.md"), sharedSkillBody)
	writeFragmentTestFile(t, filepath.Join(producerSrc, "manifest.yaml"), fmt.Sprintf("name: %s\nschema_version: 1\nenabled: false\nexports:\n  skills:\n    - id: shared-playbook\n      path: skills/shared-playbook.md\n", producer))

	consumerSrc := filepath.Join(workdir, "src-"+consumer)
	writeFragmentTestFile(t, filepath.Join(consumerSrc, "main.bot"), "workflow "+strings.ReplaceAll(consumer, "-", "_")+" {}\n")
	writeFragmentTestFile(t, filepath.Join(consumerSrc, "manifest.yaml"), fmt.Sprintf("name: %s\nschema_version: 1\nenabled: false\ndependencies:\n  skills:\n    - name: shared-playbook\n      from: %s\n", consumer, producer))

	repinFragmentLock(t, workdir, map[string]string{consumer: consumerSrc, producer: producerSrc})
	return workdir
}
