package connectors_test

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/connectors"
	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// shippedPackages lists the connector packages in this directory on disk: a
// subdirectory holding a connector.yaml, which is what makes a directory a
// package everywhere else (pkg/connection's catalog uses the same rule).
func shippedPackages(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the shipped packages: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(e.Name(), spec.ConnectorFile)); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		t.Fatal("no connector package on disk — the guard would pass by checking nothing")
	}
	return out
}

// TestEveryShippedPackageIsBaked.
//
// //go:embed names its patterns one by one, so a connector added under
// connectors/ is shipped in the tree and ABSENT from the binary unless the
// directive is edited too. Nothing on the resolution path would say so: the
// package resolves in a checkout and fails with "no such connector" on an
// installed binary, which is the failure the baked tier exists to end.
func TestEveryShippedPackageIsBaked(t *testing.T) {
	baked := connectors.FS()
	for _, id := range shippedPackages(t) {
		if _, err := fs.Stat(baked, path.Join(id, spec.ConnectorFile)); err != nil {
			t.Errorf("the shipped package %q is not in the binary: %v — add it to the //go:embed directive in embed.go", id, err)
		}
	}
}

// TestTheBakedCopyIsTheShippedOne — every file of every package, byte for
// byte.
//
// A directive matching the directory but not all of it (a pattern without
// `all:`, a nested ops/ that a narrower pattern misses) bakes a package that
// loads and is NOT the one in the tree: an operation reviewed in a PR and a
// different one served at run time. Compared in both directions, so a file
// baked that the tree does not have is caught too — that would be a stale
// copy nobody can review.
func TestTheBakedCopyIsTheShippedOne(t *testing.T) {
	baked := connectors.FS()
	for _, id := range shippedPackages(t) {
		onDisk := map[string][]byte{}
		err := filepath.WalkDir(id, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			onDisk[filepath.ToSlash(p)] = body
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", id, err)
		}

		inBinary := map[string][]byte{}
		err = fs.WalkDir(baked, id, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			body, err := fs.ReadFile(baked, p)
			if err != nil {
				return err
			}
			inBinary[p] = body
			return nil
		})
		if err != nil {
			t.Fatalf("walk the baked %s: %v", id, err)
		}

		for p, want := range onDisk {
			got, ok := inBinary[p]
			if !ok {
				t.Errorf("%s is in the tree and not in the binary — the //go:embed directive does not cover it", p)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs between the tree and the binary (%d bytes baked, %d on disk)", p, len(got), len(want))
			}
		}
		for p := range inBinary {
			if _, ok := onDisk[p]; !ok {
				t.Errorf("%s is in the binary and not in the tree — nobody can review what a run would get", p)
			}
		}
	}
}

// TestEveryBakedFileIsTracked.
//
// `all:` embeds the directory as the BUILD found it, dotfiles and untracked
// scratch included, so a build from a dirty checkout would bake bytes no
// commit holds and no reviewer ever reads — while the package still loads and
// still answers. What a run gets has to be what the tree says, which is the
// same reason the bots catalog guard reads git rather than the working
// directory.
func TestEveryBakedFileIsTracked(t *testing.T) {
	baked := connectors.FS()
	for _, id := range shippedPackages(t) {
		out, err := gittest.Cmd(".", "ls-files", "-z", "--", id).Output()
		if err != nil {
			t.Fatalf("git ls-files %s: %v", id, err)
		}
		tracked := map[string]bool{}
		for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
			if rel != "" {
				tracked[filepath.ToSlash(rel)] = true
			}
		}
		if len(tracked) == 0 {
			t.Fatalf("git tracks no file under %s — the guard would pass by checking nothing", id)
		}
		err = fs.WalkDir(baked, id, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if !tracked[p] {
				t.Errorf("%s is baked into the binary and not tracked by git — the build read a dirty tree", p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk the baked %s: %v", id, err)
		}
	}
}
