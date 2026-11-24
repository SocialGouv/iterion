package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemContextIdentityFollowsTheEffectiveRoot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	relative, err := New("runs-store")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := New(filepath.Join(root, "runs-store"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := ResolveStoreContextRef(t.Context(), abs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveStoreContextRef(t.Context(), relative)
	if err != nil || got != want || !HasStoreContextIdentity(got) {
		t.Fatalf("relative identity=%+v err=%v, want %+v", got, err, want)
	}
	if err := os.Symlink(filepath.Join(root, "runs-store"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	alias, err := New("alias")
	if err != nil {
		t.Fatal(err)
	}
	got, err = ResolveStoreContextRef(t.Context(), alias)
	if err != nil || got != want {
		t.Fatalf("symlink identity=%+v err=%v, want %+v", got, err, want)
	}
	other, err := New("other")
	if err != nil {
		t.Fatal(err)
	}
	different, err := ResolveStoreContextRef(t.Context(), other)
	if err != nil || different.ID == want.ID {
		t.Fatalf("distinct filesystem roots collapse: %+v err=%v", different, err)
	}
}

type contextIdentityLegacyStore struct{ RunStore }

func (contextIdentityLegacyStore) Root() string { return "legacy-root" }

type contextIdentityDecorator struct{ RunStore }

func (s contextIdentityDecorator) Unwrap() RunStore { return s.RunStore }

func TestContextIdentityCapabilityAndLegacyMigration(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want, err := ResolveStoreContextRef(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveStoreContextRef(t.Context(), contextIdentityDecorator{s})
	if err != nil || got != want {
		t.Fatalf("decorator changes identity: %+v err=%v want %+v", got, err, want)
	}
	legacy, err := ResolveStoreContextRef(context.Background(), contextIdentityLegacyStore{})
	if err != nil || HasStoreContextIdentity(legacy) || legacy.Namespace != "legacy-root" {
		t.Fatalf("unknown store falsely claims comparability: %+v err=%v", legacy, err)
	}
	c := &ExecutionContext{Version: 1, RunStore: legacy}
	if err := c.Normalize(); err != nil {
		t.Fatalf("legacy reference is no longer readable: %v", err)
	}
}

func TestFilesystemContextIdentityKeepsSignificantPathBytes(t *testing.T) {
	root := t.TempDir()
	seen := map[string]string{}
	for _, name := range []string{"store", "store ", "store\t", "store\u00a0"} {
		s, err := New(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		ref, err := s.ContextIdentity(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if previous, ok := seen[ref.ID]; ok {
			t.Errorf("distinct filesystem roots %q and %q collapse", previous, name)
		}
		seen[ref.ID] = name
	}
	// The encoding is byte-preserving even when a namespace is not UTF-8,
	// and its component boundary is unambiguous.
	for _, pair := range [][2]ContextRef{
		{NewStoreContextRef("filesystem", "store\xff"), NewStoreContextRef("filesystem", "store\xfe")},
		{NewStoreContextRef("a", "bc"), NewStoreContextRef("ab", "c")},
	} {
		if pair[0].ID == pair[1].ID {
			t.Errorf("distinct exact namespace components collapse: %+v", pair)
		}
	}
}
