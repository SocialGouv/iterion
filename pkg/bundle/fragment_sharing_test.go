package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAFragmentSharingManifestDecodesBothSides proves the two-sided key is
// read, normalized and defaulted: a producer-only key cannot express
// consumption, so both halves must survive the decode.
func TestAFragmentSharingManifestDecodesBothSides(t *testing.T) {
	m, err := DecodeManifest([]byte(`name: app-dev
schema_version: 1
exports:
  fragments:
    - id: forge-auth
      path: lib/forge-auth.bot
dependencies:
  fragments:
    - name: forge-auth
      from: app-dev
    - name: house-schemas
      from: app-dev
      id: schemas
`), "manifest.yaml")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(m.Exports.Fragments); got != 1 {
		t.Fatalf("exports.fragments = %d, want 1", got)
	}
	if got := m.Exports.Fragments[0]; got.ID != "forge-auth" || got.Path != "lib/forge-auth.bot" {
		t.Fatalf("export = %+v", got)
	}
	if got := len(m.Dependencies.Fragments); got != 2 {
		t.Fatalf("dependencies.fragments = %d, want 2", got)
	}
	// The exported id defaults to the consumer-side name, and the
	// materialized path is the one an `import` already spells.
	if got := m.Dependencies.Fragments[0].ExportedID(); got != "forge-auth" {
		t.Fatalf("ExportedID with no id = %q, want %q", got, "forge-auth")
	}
	if got := m.Dependencies.Fragments[0].FragmentRelPath(); got != "lib/forge-auth.bot" {
		t.Fatalf("FragmentRelPath = %q, want %q", got, "lib/forge-auth.bot")
	}
	if got := m.Dependencies.Fragments[1].ExportedID(); got != "schemas" {
		t.Fatalf("ExportedID with an explicit id = %q, want %q", got, "schemas")
	}
}

// TestFragmentSharingIsRefusedPerIndex pins one refusal per rule, each
// naming the index an author reads in the file. Table-driven on purpose: a
// rule that stops refusing is a silently-accepted declaration.
func TestFragmentSharingIsRefusedPerIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"export with no id", "exports:\n  fragments:\n    - path: lib/x.bot\n", "exports.fragments[0].id is required"},
		{"export id with a separator", "exports:\n  fragments:\n    - id: a/b\n      path: lib/x.bot\n", `exports.fragments[0].id "a/b" must not contain path separators`},
		{"duplicated export id", "exports:\n  fragments:\n    - id: x\n      path: lib/x.bot\n    - id: x\n      path: lib/y.bot\n", `exports.fragments[1].id "x" is duplicated`},
		{"export with no path", "exports:\n  fragments:\n    - id: x\n", "exports.fragments[0].path is required"},
		{"export path climbing out", "exports:\n  fragments:\n    - id: x\n      path: ../other/lib/x.bot\n", "must stay inside the bundle"},
		{"export path that is not a fragment", "exports:\n  fragments:\n    - id: x\n      path: lib/x.md\n", "must name a .bot fragment"},
		{"export path outside lib", "exports:\n  fragments:\n    - id: x\n      path: frag/x.bot\n", "must live under lib/"},
		{"dependency with no name", "dependencies:\n  fragments:\n    - from: app-dev\n", "dependencies.fragments[0].name is required"},
		{"dependency name with a separator", "dependencies:\n  fragments:\n    - name: a/b\n      from: app-dev\n", "must not contain path separators"},
		{"dependency name carrying the extension", "dependencies:\n  fragments:\n    - name: x.bot\n      from: app-dev\n", "must not carry the .bot extension"},
		{"dependency name with whitespace", "dependencies:\n  fragments:\n    - name: \"a b\"\n      from: app-dev\n", "must not contain whitespace or control characters"},
		{"duplicated dependency name", "dependencies:\n  fragments:\n    - name: x\n      from: a\n    - name: x\n      from: b\n", `dependencies.fragments[1].name "x" is duplicated`},
		{"dependency with no from", "dependencies:\n  fragments:\n    - name: x\n", "dependencies.fragments[0].from is required"},
		{"dependency from with a separator", "dependencies:\n  fragments:\n    - name: x\n      from: ../b\n", `dependencies.fragments[0].from "../b" must not contain path separators`},
		{"dependency id with a separator", "dependencies:\n  fragments:\n    - name: x\n      from: a\n      id: ../y\n", `dependencies.fragments[0].id "../y" must not contain path separators`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeManifest([]byte("name: app-dev\nschema_version: 1\n"+tc.body), "manifest.yaml")
			if err == nil {
				t.Fatalf("decode accepted %q", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestAnUnknownFragmentSharingKeyIsRefused proves the strict decode still
// holds for the new field: a typo under `fragments:` is not silently
// dropped, which is what makes an older binary refuse this key loudly.
func TestAnUnknownFragmentSharingKeyIsRefused(t *testing.T) {
	_, err := DecodeManifest([]byte("name: app-dev\nschema_version: 1\nexports:\n  fragments:\n    - id: x\n      pth: lib/x.bot\n"), "manifest.yaml")
	if err == nil {
		t.Fatal("decode accepted an unknown key under exports.fragments")
	}
}

// TestAMaterializedFragmentLeavesTheContentHashWhereItWas is the witness of
// ARB-106's answer: `bots.lock` pins what a PRODUCER published, and
// ContentHashDir is the verdict every pin check reads. Resolving a declared
// dependency must not move it, or an idempotent reconciliation reads as
// drift (the bot:// resolver refuses the consumer for good, and `bots sync`
// can never short-circuit again).
func TestAMaterializedFragmentLeavesTheContentHashWhereItWas(t *testing.T) {
	dir := t.TempDir()
	writeFragmentFixture(t, dir, "main.bot", "workflow consumer {}\n")
	writeFragmentFixture(t, dir, "manifest.yaml", "name: consumer\nschema_version: 1\n")
	shipped, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte("schema shared {}\n")
	writeFragmentFixture(t, dir, filepath.Join(DirFragments, "shared.bot"), string(body))
	withFragment, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if withFragment == shipped {
		t.Fatal("an unrecorded file under lib/ left the content hash alone; the note is not what excludes it")
	}

	if err := WriteLocalStateOfKind(dir, LocalStateFragment, []LocalStateEntry{{Path: "lib/shared.bot", SHA256: SHA256Bytes(body)}}); err != nil {
		t.Fatal(err)
	}
	recorded, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if recorded != shipped {
		t.Fatalf("hash with a recorded fragment = %s, want the shipped hash %s", recorded, shipped)
	}

	// Edited after the install, the note no longer describes the file: it is
	// hashed back IN, because the bundle on disk is no longer what any
	// producer shipped and the pin check must see that.
	writeFragmentFixture(t, dir, filepath.Join(DirFragments, "shared.bot"), "schema shared { tampered }\n")
	tampered, err := ContentHashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tampered == shipped {
		t.Fatal("an edited materialized fragment stayed out of the hash")
	}
}

// TestTheMaterializedFragmentNoteOnlyHidesFragments keeps the note's reach
// to the class of files it was written for: a note that could exclude
// main.bot would be a hash with no basis.
func TestTheMaterializedFragmentNoteOnlyHidesFragments(t *testing.T) {
	dir := t.TempDir()
	writeFragmentFixture(t, dir, "main.bot", "workflow consumer {}\n")
	writeFragmentFixture(t, dir, "manifest.yaml", "name: consumer\nschema_version: 1\n")
	if err := os.WriteFile(filepath.Join(dir, LocalStateFile), []byte(SHA256Bytes([]byte("workflow consumer {}\n"))+" main.bot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ContentHashDir(dir)
	if err == nil {
		t.Fatal("ContentHashDir honoured a note that named main.bot")
	}
	if !strings.Contains(err.Error(), "is not a path an install writes") {
		t.Fatalf("error = %q", err)
	}
}

// TestAnUnreadableMaterializedFragmentNoteIsAnError refuses a silent
// fallback: a hash computed over a record nobody could read is a verdict
// with no basis.
func TestAnUnreadableMaterializedFragmentNoteIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFragmentFixture(t, dir, "main.bot", "workflow consumer {}\n")
	if err := os.WriteFile(filepath.Join(dir, LocalStateFile), []byte("not-a-digest lib/x.bot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ContentHashDir(dir); err == nil {
		t.Fatal("ContentHashDir accepted a malformed note")
	}
	if _, err := ReadLocalState(dir); err == nil {
		t.Fatal("ReadLocalState accepted a malformed note")
	}
}

// TestAnEmptyMaterializedFragmentListRemovesTheNote keeps a bundle whose
// dependencies were all dropped hashing as the producer shipped it.
func TestAnEmptyMaterializedFragmentListRemovesTheNote(t *testing.T) {
	dir := t.TempDir()
	body := []byte("schema shared {}\n")
	writeFragmentFixture(t, dir, filepath.Join(DirFragments, "shared.bot"), string(body))
	if err := WriteLocalStateOfKind(dir, LocalStateFragment, []LocalStateEntry{{Path: "lib/shared.bot", SHA256: SHA256Bytes(body)}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteLocalStateOfKind(dir, LocalStateFragment, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, LocalStateFile)); !os.IsNotExist(err) {
		t.Fatalf("note still there: %v", err)
	}
	frags, err := ReadLocalState(dir)
	if err != nil || frags != nil {
		t.Fatalf("ReadLocalState = %v, %v; want nil, nil", frags, err)
	}
}

func writeFragmentFixture(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
