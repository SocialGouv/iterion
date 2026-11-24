package bundle

import (
	"path/filepath"
	"strings"
	"testing"
)

// A dependency name becomes a PATH the install writes and a line in the
// bundle's local-state record, so a name no install could materialize is
// refused where the declaration is read — not halfway through a sync, with
// a file already on disk. Per index, like every other rule of the key.
func TestADependencyNameThatNoInstallCouldMaterializeIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		dependencies string
		want         string
	}{
		"a name with a space": {
			"    - name: shared playbook\n      from: app-dev\n",
			`dependencies.skills[0].name "shared playbook" must not contain whitespace or control characters`,
		},
		"a name carrying the extension": {
			"    - name: verify-build.md\n      from: app-dev\n",
			`dependencies.skills[0].name "verify-build.md" must not carry the .md extension: it names the skill a ` + "`skills:`" + ` list spells, which the install materializes at skills/verify-build.md`,
		},
		"the generated catalogue's name": {
			"    - name: iterion-bot-catalog\n      from: app-dev\n",
			`dependencies.skills[0].name "iterion-bot-catalog" is the generated bot catalog at skills/iterion-bot-catalog.md`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeManifest([]byte(skillSharingManifest("", tc.dependencies)), "manifest.yaml")
			if err == nil {
				t.Fatalf("the manifest decoded: nothing refused %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not name the entry and the rule:\ngot:  %v\nwant: %s", err, tc.want)
			}
		})
	}
}

// Where the install writes a resolved dependency is ONE answer, read by the
// resolution, the record's shape test and every refusal that names the path.
func TestASkillDependencyKnowsWhereItMaterializes(t *testing.T) {
	dep := SkillDependency{Name: "verify-build", From: "app-dev"}
	if got := dep.SkillRelPath(); got != "skills/verify-build.md" {
		t.Errorf("SkillRelPath() = %q, want skills/verify-build.md", got)
	}
	if got := dep.ExportedID(); got != "verify-build" {
		t.Errorf("ExportedID() = %q, want the name it is referenced under", got)
	}
	renamed := SkillDependency{Name: "local-alias", From: "app-dev", ID: "verify-build"}
	if got := renamed.ExportedID(); got != "verify-build" {
		t.Errorf("ExportedID() = %q, want the declared id", got)
	}
	if got := renamed.SkillRelPath(); got != "skills/local-alias.md" {
		t.Errorf("SkillRelPath() = %q: a dependency materializes under the name THIS bundle references", got)
	}
}

// TestAMaterializedSkillIsOutOfTheHash is why the resolution is recorded at
// all: the document the install added is not part of what the producer
// published, so a consumer that resolved a declared skill still hashes to
// the pin `bots.lock` holds — otherwise the post-install verification
// refuses the install and every later sync re-installs forever.
func TestAMaterializedSkillIsOutOfTheHash(t *testing.T) {
	dir := t.TempDir()
	writeFragmentFixture(t, dir, MainBotFile, "workflow consumer {}\n")
	writeFragmentFixture(t, dir, ManifestFile, "name: consumer\nschema_version: 1\n")
	pin, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	resolved := []byte("---\nname: shared-playbook\ndescription: resolved.\n---\n\nbody\n")
	rel := DirSkills + "/shared-playbook.md"
	writeFragmentFixture(t, dir, filepath.FromSlash(rel), string(resolved))
	if err := WriteLocalStateOfKind(dir, LocalStateSkill, []LocalStateEntry{
		{Path: rel, SHA256: SHA256Bytes(resolved)},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != pin {
		t.Fatalf("hash with a recorded skill = %s, want the pinned %s: resolving a declared dependency reads as drift", got, pin)
	}

	// Edited after the install: the record no longer describes the file, so
	// the hash MOVES. The install on disk is not what any producer shipped,
	// and a pin check that said otherwise would be lying.
	writeFragmentFixture(t, dir, filepath.FromSlash(rel), "edited by hand\n")
	edited, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if edited == pin {
		t.Fatal("an edited skill still hashes as the producer's content: the record hid a file it no longer describes")
	}
}

// The generated catalogue is a `.md` directly under skills/ too, and it is
// the OTHER kind — a replaced path with the producer's bytes kept aside. One
// line, one writer: classifying it as a skill would hand the catalogue's
// line to the resolution's reconciliation, which would drop it.
func TestTheGeneratedCatalogueIsNotReadAsAResolvedSkill(t *testing.T) {
	dir := t.TempDir()
	err := WriteLocalStateOfKind(dir, LocalStateSkill, []LocalStateEntry{
		{Path: catalogRelPath, SHA256: SHA256Bytes([]byte("x"))},
	})
	if err == nil {
		t.Fatal("the skill writer accepted the generated catalogue's line")
	}
	if !strings.Contains(err.Error(), "is catalog local state, not skill") {
		t.Errorf("error = %q", err)
	}
}

// The record describes what this tree WRITES and nothing else: a path the
// install has no way of producing is refused rather than honoured, because
// honouring it would hide an arbitrary file from every pin check.
func TestARecordedSkillPathOutsideTheMirroredDirectoryIsRefused(t *testing.T) {
	for name, path := range map[string]string{
		"a nested document":      DirSkills + "/nested/shared.md",
		"another extension":      DirSkills + "/shared.txt",
		"outside the skills dir": "docs/shared.md",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			err := WriteLocalStateOfKind(dir, LocalStateSkill, []LocalStateEntry{
				{Path: path, SHA256: SHA256Bytes([]byte("x"))},
			})
			if err == nil {
				t.Fatalf("the record accepted %q", path)
			}
			if !strings.Contains(err.Error(), "is not a path an install writes") {
				t.Fatalf("error = %q", err)
			}
		})
	}
}
