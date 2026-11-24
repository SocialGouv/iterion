package store

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/internal/testfs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scopeFixture(t *testing.T, h RunDataHeader) (context.Context, IncarnationDataScope) {
	t.Helper()
	b, e := CaptureRunDataBinding(h)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := WithRunDataBinding(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	s, e := ResolveIncarnationDataScope(ctx, h)
	if e != nil {
		t.Fatal(e)
	}
	return ctx, s
}
func TestIncarnationDataScopeRetainsOriginalIdentity(t *testing.T) {
	h := RunDataHeader{RunID: "r", BirthID: "birth-a", Format: LegacyValueFormat, Protocol: IncarnationRunData}
	ctx, s := scopeFixture(t, h)
	h.BirthID = "birth-b"
	if _, e := ResolveIncarnationDataScope(ctx, h); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatalf("adopted successor: %v", e)
	}
	copy := s.Header()
	copy.BirthID = "changed"
	prefix, e := s.BlobPrefix()
	if e != nil || prefix != "run-data-v1/f1/r/birth-a/" {
		t.Fatalf("redirected scope: %q %v", prefix, e)
	}
	if _, e := ResolveIncarnationDataScope(t.Context(), h); !errors.Is(e, ErrRunDataBindingRequired) {
		t.Fatalf("unbound scope: %v", e)
	}
	if _, e := (IncarnationDataScope{}).FilesystemRoot(t.TempDir()); !errors.Is(e, ErrRunDataBindingRequired) {
		t.Fatalf("zero scope: %v", e)
	}
	if _, e := (IncarnationDataScope{}).BlobPrefix(); !errors.Is(e, ErrRunDataBindingRequired) {
		t.Fatalf("zero prefix: %v", e)
	}
}

// This is a naming-contract proof, not a claim that store payload APIs have
// migrated to these roots. Backend IO and cleanup integration remain separate.
func TestIncarnationDataScopeDisjointPhysicalRoots(t *testing.T) {
	root := t.TempDir()
	h := RunDataHeader{RunID: "r", BirthID: "birth-a", Format: LegacyValueFormat, Protocol: IncarnationRunData}
	_, old := scopeFixture(t, h)
	h.BirthID = "birth-a-extra"
	_, current := scopeFixture(t, h)
	oldRoot, e := old.FilesystemRoot(root)
	if e != nil {
		t.Fatal(e)
	}
	newRoot, e := current.FilesystemRoot(root)
	if e != nil {
		t.Fatal(e)
	}
	for _, dir := range []string{oldRoot, newRoot, filepath.Join(root, "runs", "r")} {
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(filepath.Join(newRoot, "payload"), []byte("new"), 0600); e != nil {
		t.Fatal(e)
	}
	testfs.RemoveAll(t, root, filepath.Join(root, "runs", "r"))
	if e := os.WriteFile(filepath.Join(oldRoot, "payload"), []byte("late-old"), 0600); e != nil {
		t.Fatal(e)
	}
	testfs.RemoveAll(t, root, oldRoot)
	b, e := os.ReadFile(filepath.Join(newRoot, "payload"))
	if e != nil || string(b) != "new" {
		t.Fatalf("successor affected: %q %v", b, e)
	}
	a, e := old.BlobPrefix()
	if e != nil {
		t.Fatal(e)
	}
	bprefix, e := current.BlobPrefix()
	if e != nil {
		t.Fatal(e)
	}
	if strings.HasPrefix(bprefix, a) || strings.HasPrefix(a, bprefix) {
		t.Fatalf("overlapping cleanup prefixes: %q %q", a, bprefix)
	}
	h.Format = ExactValueFormat
	_, exact := scopeFixture(t, h)
	exactRoot, e := exact.FilesystemRoot(root)
	if e != nil {
		t.Fatal(e)
	}
	exactPrefix, e := exact.BlobPrefix()
	if e != nil {
		t.Fatal(e)
	}
	if exactRoot == newRoot || exactPrefix == bprefix {
		t.Fatal("numeric formats share a data namespace")
	}
	for _, legacy := range []string{"artifacts/", "artifacts-v2/", "attachments/", "tools/", "runfiles/", "sessions/", "ir/"} {
		if strings.HasPrefix(bprefix, legacy) {
			t.Fatalf("legacy cleanup overlaps %q", legacy)
		}
	}
}
func TestIncarnationDataScopeRejectsUnsafeComponents(t *testing.T) {
	for _, bad := range []string{"../other", "a/b", "a\\b", "."} {
		t.Run(bad, func(t *testing.T) {
			h := RunDataHeader{RunID: "r", BirthID: bad, Format: LegacyValueFormat, Protocol: IncarnationRunData}
			b, e := CaptureRunDataBinding(h)
			if e != nil {
				return
			}
			ctx, e := WithRunDataBinding(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			if _, e := ResolveIncarnationDataScope(ctx, h); e == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}
