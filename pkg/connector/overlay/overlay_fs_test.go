package overlay_test

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// theAuthoredHalf is the minimum that makes the fixture's package usable: the
// description declares no security scheme at all, so the auth the overlay
// states is the difference between a package that validates and one that
// cannot.
const theAuthoredHalf = `schema_version: 1
connector: probe
auth:
  - id: token
    kind: api_key
    in: header
    name: Authorization
`

// packageInAnFS writes the generated fixture plus the overlay bytes to a temp
// directory and mirrors the result into an in-memory filesystem under at.
//
// An fstest.MapFS, because the property is that the loader no longer knows
// what a filesystem is: a second temp directory would pass against the
// os-coupled loader this seam replaces.
func packageInAnFS(t *testing.T, at, overlayBody string) fstest.MapFS {
	t.Helper()
	dir := t.TempDir()
	if err := spec.Write(dir, generated(t)); err != nil {
		t.Fatalf("write the generated half: %v", err)
	}
	if overlayBody != "" {
		if err := os.WriteFile(filepath.Join(dir, overlay.File), []byte(overlayBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := fstest.MapFS{}
	root := filepath.Clean(dir)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
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

// TestLoadPackageFSCarriesTheAuthoredHalfToo.
//
// LoadPackage is the loader a LAUNCH uses, and the whole point of the
// generated/authored split is that neither half is usable alone. The io/fs
// seam has to carry BOTH: a package served from the binary with its overlay
// skipped would answer to the derived operation ids instead of the pinned
// ones and run with the vendor's auth instead of the one iterion states —
// green in validation, wrong at run time.
func TestLoadPackageFSCarriesTheAuthoredHalfToo(t *testing.T) {
	fsys := packageInAnFS(t, "catalog/probe", theAuthoredHalf)

	// The generated half alone reads, and legitimately fails the complete
	// check: that is the state the split exists for, and it is what makes
	// this test able to tell the two loaders apart.
	raw, err := spec.LoadGeneratedFS(fsys, "catalog/probe")
	if err != nil {
		t.Fatalf("the generated half must read through io/fs: %v", err)
	}
	if err := raw.Validate(); err == nil {
		t.Fatal("the fixture must be unusable without its overlay, or this test cannot tell the halves apart")
	}

	merged, err := overlay.LoadPackageFS(fsys, "catalog/probe")
	if err != nil {
		t.Fatalf("LoadPackageFS: %v", err)
	}
	if len(merged.Connector.Auth) != 1 || merged.Connector.Auth[0].ID != "token" {
		t.Errorf("auth = %+v, want the single scheme the overlay states", merged.Connector.Auth)
	}
	if len(merged.Operations()) == 0 {
		t.Error("the merged package carries no operation — the generated half did not travel")
	}
}

// TestAnAbsentOverlayStaysAbsent is the falsifier of the refusal below: most
// packages have no authored half, and a loader that demanded one would
// invalidate every package whose description says everything.
func TestAnAbsentOverlayStaysAbsent(t *testing.T) {
	fsys := packageInAnFS(t, "catalog/probe", "")
	ov, err := overlay.LoadFS(fsys, "catalog/probe")
	if err != nil {
		t.Fatalf("an absent overlay is not an error: %v", err)
	}
	if ov != nil {
		t.Errorf("overlay = %+v, want none", ov)
	}
}

// TestAnUnaddressableDirectoryIsNotAnAbsentOverlay.
//
// fstest.MapFS answers "does not exist" for a path io/fs cannot address
// (absolute, dot-dot), and that answer reaching the absent clause would read
// as "this package has no overlay" — serving the generated half UNCORRECTED
// while the load looked clean. The same shape as the tier that is present and
// unreadable: collapsing it with absence turns validation into selection.
func TestAnUnaddressableDirectoryIsNotAnAbsentOverlay(t *testing.T) {
	fsys := packageInAnFS(t, "catalog/probe", theAuthoredHalf)
	for _, dir := range []string{"/catalog/probe", "../probe", ""} {
		ov, err := overlay.LoadFS(fsys, dir)
		if err == nil {
			t.Errorf("dir %q: overlay = %+v with no error — an unaddressable path must be refused, not read as an absent overlay", dir, ov)
		}
	}
}
