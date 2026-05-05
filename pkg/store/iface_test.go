package store

import "testing"

// Compile-time assertion that *FilesystemRunStore satisfies the
// RunStore interface. If a method is added to the interface and the
// FS impl doesn't supply it, this line fails the build before tests
// run — exactly the canary the cloud-ready plan §F T-04 calls for.
var _ RunStore = (*FilesystemRunStore)(nil)

// Compile-time assertion that *FilesystemRunStore implements the
// optional PIDStore interface. Cloud (Mongo) stores will not.
var _ PIDStore = (*FilesystemRunStore)(nil)

// TestRunStoreInterface_FilesystemImplementsIt is a no-op that exists
// only to give `go test` a name to report when the static asserts
// above pass. The real check is done at compile time.
func TestRunStoreInterface_FilesystemImplementsIt(t *testing.T) {
	t.Helper()
	if _, ok := any((*FilesystemRunStore)(nil)).(RunStore); !ok {
		t.Fatal("*FilesystemRunStore should implement RunStore")
	}
	if _, ok := any((*FilesystemRunStore)(nil)).(PIDStore); !ok {
		t.Fatal("*FilesystemRunStore should implement PIDStore")
	}
}
