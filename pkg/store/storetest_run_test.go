package store_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// TestConformance_FilesystemShared runs the EXPORTED cross-backend
// conformance suite (pkg/store/storetest) against the filesystem backend.
//
// pkg/store/conformance_test.go already runs an internal copy of the core
// assertions, but the shared suite is the one the Mongo backend plugs into
// — and it additionally covers the optional seams (DeleteRun, RunLogStore,
// TurnStore, …). Running the SAME harness against the filesystem store
// here means every seam the cloud twin must satisfy is proven identical on
// both backends, in-tree, without a live Mongo.
func TestConformance_FilesystemShared(t *testing.T) {
	storetest.RunWithOpts(t, func(t *testing.T) store.RunStore {
		t.Helper()
		s, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		return s
	}, storetest.Opts{InitialStatus: store.RunStatusRunning})
}
