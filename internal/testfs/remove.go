// Package testfs removes only scratch paths inside a test-owned root.
package testfs

import (
	"os"
	"path/filepath"
	"testing"
)

func RemoveAll(t testing.TB, root, target string) {
	t.Helper()
	rel, err := filepath.Rel(root, target)
	if err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("remove target %q is outside test scratch %q", target, root)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
}
