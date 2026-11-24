package spec_test

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// mirrorIntoMapFS copies the package written at dir into an in-memory
// filesystem, under the slash path at.
//
// An fstest.MapFS rather than a second temp directory, because the property
// under test is that the loader no longer knows what a filesystem IS: a test
// that read a different directory would pass against an os-coupled loader.
func mirrorIntoMapFS(t *testing.T, dir, at string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	root := filepath.Clean(dir)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[path.Join(at, filepath.ToSlash(rel))] = &fstest.MapFile{Data: body}
		return nil
	})
	if err != nil {
		t.Fatalf("mirror %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("mirror %s copied nothing — the test would prove nothing", dir)
	}
	return out
}

// theReferencePackageWithEveryDocument writes the reference package with all
// five documents a loader reads — connector.yaml, schemas.yaml, ops/*.yaml,
// responses.json and triggers.yaml — and returns the directory.
//
// All five, because a seam that converted four of the reads and left the
// fifth on os would pass any fixture missing that one, and the half a loader
// forgets to read is the half that goes silently inert.
func theReferencePackageWithEveryDocument(t *testing.T) string {
	t.Helper()
	dir := writeContractPackage(t)
	writeTriggers(t, dir, `schema_version: 1
connector: probe
triggers:
  - id: probe.issue.commented
    event: issue_comment
    description: a comment happened
`)
	return dir
}

// TestLoadGeneratedFSReadsEveryDocumentThroughIoFS.
//
// The loaders were coupled to os (os.ReadFile, os.ReadDir, os.Open), so a
// package that does not live in a directory — the copies compiled into the
// binary, a package rebuilt from a store — could only be served by a SECOND
// loader. Two loaders of one format is the defect this package already paid
// for once: the catalog read the generated half while `connectors validate`
// reported on the merged one, and the two disagreed about everything the
// authored half exists to say.
func TestLoadGeneratedFSReadsEveryDocumentThroughIoFS(t *testing.T) {
	dir := theReferencePackageWithEveryDocument(t)

	// The directory form FIRST, so the two readings are compared rather than
	// asserted twice: what the seam promises is the SAME package either way.
	onDisk, err := spec.LoadGenerated(dir)
	if err != nil {
		t.Fatalf("load from the directory: %v", err)
	}

	fsys := mirrorIntoMapFS(t, dir, "catalog/probe")
	inMemory, err := spec.LoadGeneratedFS(fsys, "catalog/probe")
	if err != nil {
		t.Fatalf("load through io/fs: %v", err)
	}

	// One assertion per document, so a failure names the read that stayed
	// behind rather than only "they differ".
	if inMemory.Connector.ID != "probe" || inMemory.Connector.Version != onDisk.Connector.Version {
		t.Errorf("connector.yaml: %+v, want the identity the directory form read (%+v)", inMemory.Connector, onDisk.Connector)
	}
	if _, ok := inMemory.Schemas["Issue"]; !ok {
		t.Errorf("schemas.yaml was not read through io/fs: schemas = %v", inMemory.Schemas)
	}
	if _, ok := inMemory.Operation("probe.issue.get"); !ok {
		t.Errorf("ops/issue.yaml was not read through io/fs: operations = %v", len(inMemory.Operations()))
	}
	if _, ok := inMemory.ResponseSchemas["Issue"]; !ok {
		t.Errorf("responses.json was not read through io/fs: contracts = %v", inMemory.ResponseSchemas)
	}
	if len(inMemory.Triggers) != 1 || inMemory.Triggers[0].ID != "probe.issue.commented" {
		t.Errorf("triggers.yaml was not read through io/fs: triggers = %+v", inMemory.Triggers)
	}

	// And the whole package, field for field: a document read into the wrong
	// place would satisfy every count above.
	if !reflect.DeepEqual(onDisk, inMemory) {
		t.Errorf("the package read through io/fs differs from the one read from the directory\n fs:   %+v\n disk: %+v", inMemory, onDisk)
	}

	// LoadFS runs the COMPLETE check, like Load: same package, same verdict.
	if _, err := spec.LoadFS(fsys, "catalog/probe"); err != nil {
		t.Errorf("LoadFS on a complete package: %v", err)
	}
}

// TestTheDirectoryFormStillNamesAFileThatCanBeOpened.
//
// os.DirFS rewrites a *fs.PathError's Path to the name relative to its root,
// so the bare form would report "read connector.yaml: open connector.yaml: no
// such file or directory" and send an operator looking in their own working
// directory. spec.DirFS restores the root — the one thing the wrapper is for,
// and the reason every path caller's message is unchanged.
func TestTheDirectoryFormStillNamesAFileThatCanBeOpened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nothing-here")
	_, err := spec.Load(dir)
	if err == nil {
		t.Fatal("loading a package from a directory that does not exist must fail")
	}
	if want := filepath.Join(dir, spec.ConnectorFile); !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to name %s", err, want)
	}

	// And the ABSENCE stays recognisable as one: pkg/connection falls through
	// to the next tier on exactly this, and a wrapper that replaced the error
	// instead of its path would stop a layered lookup on the first tier.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want it to stay an fs.ErrNotExist so a layered catalog can fall through", err)
	}
}

// TestAPackageDirectoryIoFSCannotAddressIsRefusedByName.
//
// fs implementations disagree about an absolute or dot-dot path: os.DirFS says
// "invalid argument", fstest.MapFS says "does not exist" — and "does not
// exist" is the one answer a loader must not be handed, because the overlay
// half reads it as "this package has no overlay" and serves the generated one
// uncorrected.
func TestAPackageDirectoryIoFSCannotAddressIsRefusedByName(t *testing.T) {
	fsys := mirrorIntoMapFS(t, theReferencePackageWithEveryDocument(t), "catalog/probe")
	for _, dir := range []string{"/catalog/probe", "../probe", "catalog/probe/", ""} {
		_, err := spec.LoadGeneratedFS(fsys, dir)
		if err == nil {
			t.Errorf("dir %q: a path io/fs cannot address must be refused", dir)
			continue
		}
		if !strings.Contains(err.Error(), "not a package directory") {
			t.Errorf("dir %q: error = %v, want the refusal to say what is wrong with the path", dir, err)
		}
	}
	// The falsifier: the addressable form of the same package loads.
	if _, err := spec.LoadGeneratedFS(fsys, "catalog/probe"); err != nil {
		t.Errorf("the addressable form must load: %v", err)
	}
}
