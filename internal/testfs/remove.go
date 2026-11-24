// Package testfs holds the filesystem gestures tests share: removing only
// scratch paths inside a test-owned root, and writing an executable no
// concurrent fork can hold open.
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
