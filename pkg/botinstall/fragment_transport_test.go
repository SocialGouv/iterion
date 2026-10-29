package botinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// sharedDeclaration is a real declaration, not a placeholder: the consumer's
// workflow references it, so a unit that did not reach the fragment does not
// compile and the witness cannot pass by accident.
const sharedDeclaration = `schema shared_state:
  ok: bool
  fail_log: string
`

// TestAConsumerCompilesAndTravelsWithAFragmentItDoesNotCarry is lot 3's
// claim, proven end to end over the real transport rather than a double.
//
// Three facts in one place, because they are one property: a bundle that
// ships no copy of a shared fragment does NOT compile; the install's
// materialization makes it compile, with the fragment inside the unit's
// identity; and the snapshot a run travels in carries those bytes. ADR-145
// promises a materialized fragment "needs no transport of its own" — this is
// what that costs nothing, measured.
func TestAConsumerCompilesAndTravelsWithAFragmentItDoesNotCarry(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	writeSharingProducer(t, filepath.Join(dest, "z-lib"), "z-lib")
	consumer := filepath.Join(dest, "a-bot")
	writeSharingConsumer(t, consumer, "a-bot", "z-lib")
	mainPath := filepath.Join(consumer, "main.bot")

	// 1. It carries no copy, so the unit refuses the import by name and the
	//    shared declaration reaches no program. The red baseline of the
	//    whole claim: without it, step 2 proves nothing. The refusal is the
	//    E046 the loader already had — ADR-145 mints no new code, because at
	//    parse time the file is either there or absent.
	before, err := unit.LoadDir(mainPath)
	if err != nil {
		t.Fatalf("LoadDir of a consumer missing its fragment: %v", err)
	}
	if !containsRel(before.Unread, "lib/shared.bot") {
		t.Fatalf("unread = %v, want lib/shared.bot: the missing fragment was not even tried", before.Unread)
	}
	if _, found := declarationNames(t, before)["shared_state"]; found {
		t.Fatal("the shared schema reached the program without any copy of the fragment")
	}
	if !hasDiagnostic(before.Diagnostics, "E046", "no such fragment") {
		t.Fatalf("diagnostics = %v, want an E046 naming the missing fragment", before.Diagnostics)
	}

	resolved, err := ResolveFragments(workdir, consumer)
	if err != nil {
		t.Fatalf("ResolveFragments: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved = %+v, want one fragment", resolved)
	}

	// 2. The same bundle, unchanged in its own source, now compiles as ONE
	//    unit whose identity covers the fragment's bytes.
	u, err := unit.LoadDir(mainPath)
	if err != nil {
		t.Fatalf("the consumer still does not load after the install materialized its dependency: %v", err)
	}
	if len(u.Unread) != 0 || len(u.Diagnostics) != 0 {
		t.Fatalf("after materialization: unread=%v diagnostics=%v, want neither", u.Unread, u.Diagnostics)
	}
	var fragmentRels []string
	for _, f := range u.Files {
		fragmentRels = append(fragmentRels, f.Rel)
	}
	if !containsRel(fragmentRels, "lib/shared.bot") {
		t.Fatalf("unit files = %v, want lib/shared.bot among them", fragmentRels)
	}
	if u.Merged == nil {
		t.Fatal("the unit has no merged program")
	}
	if _, found := declarationNames(t, u)["shared_state"]; !found {
		t.Fatalf("the shared schema is not in the merged program: %v", declarationNames(t, u))
	}
	// The unit's digest covers every file it read, so the fragment's bytes
	// are part of what a run records and `rewind --auto` diffs.
	withFragment := u.Digest
	writeInstallTestFile(t, filepath.Join(consumer, "lib", "shared.bot"), sharedDeclaration+"\n")
	after, err := unit.LoadDir(mainPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Digest == withFragment {
		t.Fatal("the unit digest ignored a change to the materialized fragment")
	}
	writeInstallTestFile(t, filepath.Join(consumer, "lib", "shared.bot"), sharedDeclaration)

	// 3. The snapshot a run travels in carries the materialized bytes: the
	//    fragment needs no transport of its own because it IS a file of the
	//    bundle by then.
	snap := &bundle.Snapshot{}
	if err := snap.AddDir("a-bot", consumer); err != nil {
		t.Fatalf("Snapshot.AddDir: %v", err)
	}
	file, ok := snap.Files["a-bot/lib/shared.bot"]
	if !ok {
		var keys []string
		for k := range snap.Files {
			keys = append(keys, k)
		}
		t.Fatalf("the snapshot does not carry the materialized fragment; it holds %v", keys)
	}
	if string(file.Content) != sharedDeclaration {
		t.Fatalf("snapshot bytes = %q, want the producer's %q", file.Content, sharedDeclaration)
	}
	if got := bundle.SHA256Bytes(file.Content); got != resolved[0].SHA256 {
		t.Fatalf("snapshot digest = %s, the install recorded %s", got, resolved[0].SHA256)
	}
}

// TestTheSnapshotRefusesASymlinkedFragment keeps the reason ADR-145 forbids a
// link rather than a copy: a bundle that resolved on one disk would fail in
// the snapshot, in cloud, and in every consumer that installed it.
func TestTheSnapshotRefusesASymlinkedFragment(t *testing.T) {
	workdir := t.TempDir()
	consumer := filepath.Join(workdir, ".botz", "a-bot")
	writeSharingConsumer(t, consumer, "a-bot", "z-lib")
	target := filepath.Join(workdir, "elsewhere.bot")
	writeInstallTestFile(t, target, sharedDeclaration)
	link := filepath.Join(consumer, "lib", "shared.bot")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	snap := &bundle.Snapshot{}
	err := snap.AddDir("a-bot", consumer)
	if err == nil {
		t.Fatal("the snapshot carried a symlinked fragment")
	}
	if !strings.Contains(err.Error(), "is not portable") {
		t.Fatalf("error = %q", err)
	}
}

// TestAnExportedSymlinkIsRefusedAtResolution closes the same door one layer
// earlier: the producer cannot hand out a link for the install to copy.
func TestAnExportedSymlinkIsRefusedAtResolution(t *testing.T) {
	workdir := t.TempDir()
	dest := filepath.Join(workdir, ".botz")
	producer := filepath.Join(dest, "z-lib")
	writeSharingProducer(t, producer, "z-lib")
	outside := filepath.Join(workdir, "outside.bot")
	writeInstallTestFile(t, outside, sharedDeclaration)
	exported := filepath.Join(producer, "lib", "shared.bot")
	if err := os.Remove(exported); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, exported); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	consumer := filepath.Join(dest, "a-bot")
	writeSharingConsumer(t, consumer, "a-bot", "z-lib")

	_, err := ResolveFragments(workdir, consumer)
	if err == nil {
		t.Fatal("ResolveFragments copied through an exported symlink")
	}
	if !strings.Contains(err.Error(), "which is not a regular file") {
		t.Fatalf("error = %q", err)
	}
}

func declarationNames(t *testing.T, u *unit.Unit) map[string]struct{} {
	t.Helper()
	names := map[string]struct{}{}
	for _, decl := range u.Merged.Schemas {
		names[decl.Name] = struct{}{}
	}
	return names
}

func containsRel(rels []string, want string) bool {
	for _, rel := range rels {
		if filepath.ToSlash(rel) == want {
			return true
		}
	}
	return false
}

func writeSharingProducer(t *testing.T, dir, name string) {
	t.Helper()
	writeInstallTestFile(t, filepath.Join(dir, "main.bot"), "compute done:\n  output: shared_state\n  expr:\n    ok: \"true\"\n    fail_log: \"''\"\n\nworkflow producer:\n  entry: done\n")
	writeInstallTestFile(t, filepath.Join(dir, "lib", "shared.bot"), sharedDeclaration)
	writeInstallTestFile(t, filepath.Join(dir, "manifest.yaml"), "name: "+name+"\nschema_version: 1\nenabled: false\nexports:\n  fragments:\n    - id: shared\n      path: lib/shared.bot\n")
}

func writeSharingConsumer(t *testing.T, dir, name, from string) {
	t.Helper()
	writeInstallTestFile(t, filepath.Join(dir, "main.bot"), "import \"lib/shared.bot\"\n\ncompute gate:\n  output: shared_state\n  expr:\n    ok: \"true\"\n    fail_log: \"''\"\n\nworkflow consumer:\n  entry: gate\n")
	writeInstallTestFile(t, filepath.Join(dir, "manifest.yaml"), "name: "+name+"\nschema_version: 1\nenabled: false\ndependencies:\n  fragments:\n    - name: shared\n      from: "+from+"\n")
}

func hasDiagnostic(diags []parser.Diagnostic, code, substring string) bool {
	for _, d := range diags {
		if string(d.Code) == code && strings.Contains(d.Message, substring) {
			return true
		}
	}
	return false
}
