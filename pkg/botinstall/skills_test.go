package botinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

const producerSkillBody = "---\nname: shared-playbook\ndescription: The gate the consumers used to each carry a copy of.\n---\n\nRun the gate.\n"

// TestInstallAloneRefusesABundleWithSharedSkills is the fragment refusal's
// twin for the other artefact kind: one install cannot resolve a dependency
// whose producer the project lockfile materializes, and resolving on the spot
// would make the verdict depend on the alphabetical order Sync happens to
// walk. The refusal names the command that does it in the right order, and
// nothing is written.
func TestInstallAloneRefusesABundleWithSharedSkills(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "src")
	writeSkillConsumerBundle(t, source, "a-bot", "z-lib", "shared-playbook")

	_, err := Install(t.Context(), Options{Source: source, Workdir: workdir, SkipCatalog: true})
	if err == nil {
		t.Fatal("Install resolved shared skills on its own")
	}
	for _, want := range []string{`bundle "a-bot" declares dependencies.skills`, "shared-playbook (from z-lib)", "iterion bots sync"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(workdir, ".botz", "a-bot")); !os.IsNotExist(statErr) {
		t.Fatalf("the refusal wrote an install anyway: %v", statErr)
	}
}

// TestDeferredSharedSkillsLetTheCallerOwnTheOrder is the other half: botdeps
// sets DeferSharedDeps because it materializes the whole lockfile first, so
// the install itself must go through — and the one flag covers every artefact
// kind of the sharing key.
func TestDeferredSharedSkillsLetTheCallerOwnTheOrder(t *testing.T) {
	workdir := t.TempDir()
	source := filepath.Join(workdir, "src")
	writeSkillConsumerBundle(t, source, "a-bot", "z-lib", "shared-playbook")

	res, err := Install(t.Context(), Options{Source: source, Workdir: workdir, SkipCatalog: true, DeferSharedDeps: true})
	if err != nil {
		t.Fatalf("Install with DeferSharedDeps: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(res.InstalledPath, "main.bot")); statErr != nil {
		t.Fatalf("the bundle was not installed: %v", statErr)
	}
	if res.Skills != 0 {
		t.Fatalf("Result.Skills = %d, want 0: the deferred resolution has not run yet, and counting it here would report knowledge the bundle does not hold", res.Skills)
	}
}

// TestResolveSkillsNamesEveryLayerOnAMiss is ADR-144 clause 4 with its effect
// asserted: the error names the bundle, the skill and every directory looked
// in, AND the skills directory is not left one file short while the caller
// reads a success. The install's exit code is the screen, not the effect.
func TestResolveSkillsNamesEveryLayerOnAMiss(t *testing.T) {
	workdir := t.TempDir()
	consumer := filepath.Join(workdir, "stage", "a-bot")
	writeSkillConsumerBundle(t, consumer, "a-bot", "z-lib", "shared-playbook")

	_, err := ResolveSkills(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveSkills succeeded with no producer anywhere")
	}
	for _, want := range []string{
		`shared skill "shared-playbook" comes from bundle "z-lib", which is not materialized`,
		filepath.Join(workdir, "stage", "z-lib"),
		filepath.Join(workdir, ".botz", "z-lib"),
		"iterion bots sync",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(consumer, "skills", "shared-playbook.md")); !os.IsNotExist(statErr) {
		t.Fatalf("the miss materialized something anyway: %v", statErr)
	}
}

// TestResolveSkillsFallsBackToTheWorkspaceLayer proves the second layer is
// real: a consumer staged into a private destination still resolves against
// the workspace's own .botz, which is what StageOne promises.
func TestResolveSkillsFallsBackToTheWorkspaceLayer(t *testing.T) {
	workdir := t.TempDir()
	writeSkillProducerBundle(t, filepath.Join(workdir, ".botz", "z-lib"), "z-lib")
	consumer := filepath.Join(workdir, "stage", "a-bot")
	writeSkillConsumerBundle(t, consumer, "a-bot", "z-lib", "shared-playbook")

	resolved, err := ResolveSkills(workdir, consumer)
	if err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Path != "skills/shared-playbook.md" {
		t.Fatalf("resolved = %+v, want one skills/shared-playbook.md", resolved)
	}
	body, readErr := os.ReadFile(filepath.Join(consumer, "skills", "shared-playbook.md"))
	if readErr != nil || string(body) != producerSkillBody {
		t.Fatalf("materialized bytes = %q %v, want the producer's", body, readErr)
	}
}

// TestResolveSkillsRefusesToShadowAShippedSkill holds ADR-144 clause 5's
// no-shadow half: a silent overwrite would make `skills: [x]` mean one thing
// in the repository and another after an install, and the shipped bytes would
// be gone from a bundle whose producer published them.
func TestResolveSkillsRefusesToShadowAShippedSkill(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeSkillProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeSkillConsumerBundle(t, consumer, "a-bot", "z-lib", "shared-playbook")
	shipped := "---\nname: shared-playbook\ndescription: Its own, authored here.\n---\n\nIts own.\n"
	writeInstallTestFile(t, filepath.Join(consumer, "skills", "shared-playbook.md"), shipped)

	_, err := ResolveSkills(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveSkills overwrote a skill the bundle ships")
	}
	if !strings.Contains(err.Error(), "would shadow skills/shared-playbook.md, which this bundle ships") {
		t.Fatalf("error = %q", err)
	}
	body, readErr := os.ReadFile(filepath.Join(consumer, "skills", "shared-playbook.md"))
	if readErr != nil || string(body) != shipped {
		t.Fatalf("the shipped skill was overwritten: %q %v", body, readErr)
	}
}

// TestResolveSkillsRefusesADocumentThatAnswersToAnotherName: a skill is
// addressed by its frontmatter `name:` as much as by its file name, so
// materializing it under a name the document contradicts hands the agent a
// file reachable reliably under neither — the silent "the bot got dumber"
// this seam exists to end. Rewriting the bytes is the token substitution
// ADR-144 leaves undecided, so the refusal names both sides.
func TestResolveSkillsRefusesADocumentThatAnswersToAnotherName(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeSkillProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	// The consumer renames the shared document on the way in: `id` names the
	// export, `name` is what its own `skills:` lists would spell.
	writeInstallTestFile(t, filepath.Join(consumer, "main.bot"), "workflow a_bot {}\n")
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\ndependencies:\n  skills:\n    - name: local-alias\n      id: shared-playbook\n      from: z-lib\n")

	_, err := ResolveSkills(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveSkills materialized a document under a name its own frontmatter contradicts")
	}
	for _, want := range []string{
		`shared skill "shared-playbook" of bundle "z-lib" carries ` + "`name: shared-playbook`",
		"declare it as `name: shared-playbook`",
		"`skills: [local-alias]`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(consumer, "skills", "local-alias.md")); !os.IsNotExist(statErr) {
		t.Fatalf("the refusal materialized the renamed document anyway: %v", statErr)
	}
}

// TestResolveSkillsIsIdempotent: a second pass over an unchanged workspace
// re-materializes the same bytes and records the same line, which is what
// keeps a consumer at the hash its lock pins.
func TestResolveSkillsIsIdempotent(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeSkillProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeSkillConsumerBundle(t, consumer, "a-bot", "z-lib", "shared-playbook")

	first, err := ResolveSkills(workdir, consumer)
	if err != nil {
		t.Fatalf("first ResolveSkills: %v", err)
	}
	second, err := ResolveSkills(workdir, consumer)
	if err != nil {
		t.Fatalf("second ResolveSkills: %v", err)
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("second pass = %+v, want the first's %+v", second, first)
	}
}

// TestResolveSkillsRefusesToDeleteAnEditedSkill: the removal path deletes
// only what the record still describes exactly. An operator's edit to a
// document the manifest no longer declares is theirs, and this installer is
// not entitled to destroy it to tidy up.
func TestResolveSkillsRefusesToDeleteAnEditedSkill(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeSkillProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeSkillConsumerBundle(t, consumer, "a-bot", "z-lib", "shared-playbook")
	if _, err := ResolveSkills(workdir, consumer); err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	edited := "---\nname: shared-playbook\ndescription: Edited by hand after the install.\n---\n\nTheirs now.\n"
	writeInstallTestFile(t, filepath.Join(consumer, "skills", "shared-playbook.md"), edited)
	// The dependency is dropped, so the pass would otherwise remove the file.
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\n")

	_, err := ResolveSkills(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveSkills deleted a document it no longer described")
	}
	if !strings.Contains(err.Error(), "was edited after it was materialized and is no longer declared; remove it by hand") {
		t.Fatalf("error = %q", err)
	}
	body, readErr := os.ReadFile(filepath.Join(consumer, "skills", "shared-playbook.md"))
	if readErr != nil || string(body) != edited {
		t.Fatalf("the edited skill was destroyed: %q %v", body, readErr)
	}
}

// TestResolveSkillsLeavesABundleWithNothingDeclaredAlone: the pass writes no
// record for a bundle that declares no dependency, so a bundle an install
// left exactly as its producer shipped it keeps hashing that way.
func TestResolveSkillsLeavesABundleWithNothingDeclaredAlone(t *testing.T) {
	workdir := t.TempDir()
	consumer := filepath.Join(workdir, ".botz", "plain")
	writeInstallTestFile(t, filepath.Join(consumer, "main.bot"), "workflow plain {}\n")
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: plain\nschema_version: 1\nenabled: false\n")
	writeInstallTestFile(t, filepath.Join(consumer, "skills", "its-own.md"), producerSkillBody)

	resolved, err := ResolveSkills(workdir, consumer)
	if err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved = %+v, want none", resolved)
	}
	if _, statErr := os.Stat(filepath.Join(consumer, "skills", "its-own.md")); statErr != nil {
		t.Fatalf("the pass touched a skill the bundle ships: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(consumer, bundle.LocalStateFile)); !os.IsNotExist(statErr) {
		t.Fatalf("the pass wrote a record for a bundle that declares nothing: %v", statErr)
	}
}

// TestTheTwoSharedPassesDoNotEraseEachOthersRecord: fragments and skills are
// two kinds in ONE record, each pass reconciling its own lines. A pass that
// read the other kind's line as its own would refuse the sync over a file it
// does not own, or quietly drop the other kind's exclusion and move the hash.
func TestTheTwoSharedPassesDoNotEraseEachOthersRecord(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	// One producer exporting both artefact kinds, one consumer declaring both.
	producer := filepath.Join(dest, "z-lib")
	writeInstallTestFile(t, filepath.Join(producer, "main.bot"), "workflow z_lib {}\n")
	writeInstallTestFile(t, filepath.Join(producer, "lib", "shared.bot"), producerFragmentBody)
	writeInstallTestFile(t, filepath.Join(producer, "skills", "shared-playbook.md"), producerSkillBody)
	writeInstallTestFile(t, filepath.Join(producer, "manifest.yaml"), "name: z-lib\nschema_version: 1\nenabled: false\nexports:\n  fragments:\n    - id: shared\n      path: lib/shared.bot\n  skills:\n    - id: shared-playbook\n      path: skills/shared-playbook.md\n")
	consumer := filepath.Join(dest, "a-bot")
	writeInstallTestFile(t, filepath.Join(consumer, "main.bot"), "import \"lib/shared.bot\"\n\nworkflow a_bot {}\n")
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\ndependencies:\n  fragments:\n    - name: shared\n      from: z-lib\n  skills:\n    - name: shared-playbook\n      from: z-lib\n")

	if _, err := ResolveFragments(workdir, consumer); err != nil {
		t.Fatalf("ResolveFragments: %v", err)
	}
	if _, err := ResolveSkills(workdir, consumer); err != nil {
		t.Fatalf("ResolveSkills after the fragment pass: %v", err)
	}
	if _, err := ResolveFragments(workdir, consumer); err != nil {
		t.Fatalf("ResolveFragments after the skill pass: %v", err)
	}
	for _, kind := range []bundle.LocalStateKind{bundle.LocalStateFragment, bundle.LocalStateSkill} {
		entries, err := bundle.ReadLocalStateOfKind(consumer, kind)
		if err != nil {
			t.Fatalf("read %s record: %v", kind, err)
		}
		if len(entries) != 1 {
			t.Fatalf("%s record = %+v, want the one line its own pass wrote", kind, entries)
		}
	}
	// Both passes done, both exclusions honoured: the consumer hashes as its
	// producer published it.
	withLocalState, err := bundle.ContentHashDir(consumer)
	if err != nil {
		t.Fatal(err)
	}
	pristine := filepath.Join(workdir, "pristine")
	writeInstallTestFile(t, filepath.Join(pristine, "main.bot"), "import \"lib/shared.bot\"\n\nworkflow a_bot {}\n")
	writeInstallTestFile(t, filepath.Join(pristine, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\ndependencies:\n  fragments:\n    - name: shared\n      from: z-lib\n  skills:\n    - name: shared-playbook\n      from: z-lib\n")
	asPublished, err := bundle.ContentHashDir(pristine)
	if err != nil {
		t.Fatal(err)
	}
	if withLocalState != asPublished {
		t.Fatalf("a resolved consumer hashes %s, its producer published %s: the pin check reads a change nobody made", withLocalState, asPublished)
	}
}

func writeSkillProducerBundle(t *testing.T, dir, name string) {
	t.Helper()
	writeInstallTestFile(t, filepath.Join(dir, "main.bot"), "workflow producer {}\n")
	writeInstallTestFile(t, filepath.Join(dir, "skills", "shared-playbook.md"), producerSkillBody)
	writeInstallTestFile(t, filepath.Join(dir, "manifest.yaml"), "name: "+name+"\nschema_version: 1\nenabled: false\nexports:\n  skills:\n    - id: shared-playbook\n      path: skills/shared-playbook.md\n")
}

func writeSkillConsumerBundle(t *testing.T, dir, name, from, skill string) {
	t.Helper()
	writeInstallTestFile(t, filepath.Join(dir, "main.bot"), "workflow consumer {}\n")
	writeInstallTestFile(t, filepath.Join(dir, "manifest.yaml"), "name: "+name+"\nschema_version: 1\nenabled: false\ndependencies:\n  skills:\n    - name: "+skill+"\n      from: "+from+"\n")
}

// TestADroppedSkillDependencyIsRemovedByThePass is the removal path's own
// witness, exercised WITHOUT a re-install: a leftover copy would keep
// answering a `skills:` reference with a document the manifest no longer
// declares, which is the stale knowledge the reconciliation exists to
// prevent. A sync that re-materializes the bundle hides this branch, so the
// branch is pinned where it runs.
func TestADroppedSkillDependencyIsRemovedByThePass(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeSkillProducerBundle(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeSkillConsumerBundle(t, consumer, "a-bot", "z-lib", "shared-playbook")
	if _, err := ResolveSkills(workdir, consumer); err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	writeInstallTestFile(t, filepath.Join(consumer, "manifest.yaml"), "name: a-bot\nschema_version: 1\nenabled: false\n")

	resolved, err := ResolveSkills(workdir, consumer)
	if err != nil {
		t.Fatalf("ResolveSkills after dropping the dependency: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved = %+v, want none", resolved)
	}
	if _, statErr := os.Stat(filepath.Join(consumer, "skills", "shared-playbook.md")); !os.IsNotExist(statErr) {
		t.Fatalf("a dropped dependency still answers the reference: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(consumer, bundle.LocalStateFile)); !os.IsNotExist(statErr) {
		t.Fatalf("the record outlived the dependency it described: %v", statErr)
	}
}
