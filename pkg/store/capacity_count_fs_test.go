package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The filesystem twin of the alive capacity listing reads strictly: a run
// directory whose record cannot be read is an error, never a holder left out —
// a gateway reading that drops a holder under-counts the host it runs on. A
// deleted run is no holder and is no error.
func TestAliveCapacityListingRefusesAnUnreadableRecord(t *testing.T) {
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithTenant(context.Background(), "team")
	for _, id := range []string{"readable", "deleted", "corrupt"} {
		r, err := s.CreateRun(ctx, id, "capacity", nil)
		if err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r.Status = store.RunStatusRunning
		if err := s.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	if err := s.DeleteRun(ctx, "deleted"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	lister := store.AsAliveCapacityLister(s)
	if lister == nil {
		t.Fatal("the filesystem store must host the alive capacity listing")
	}
	if got, err := lister.ListAliveRunsForCapacity(ctx); err != nil || len(got) != 2 {
		t.Fatalf("before the corruption: %d run(s), %v; want readable and corrupt, the deleted run skipped", len(got), err)
	}
	if err := os.WriteFile(filepath.Join(root, "runs", "corrupt", "run.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := lister.ListAliveRunsForCapacity(ctx); err == nil {
		t.Fatalf("an unreadable run record was skipped: listed %d run(s) with no error", len(got))
	}
}
