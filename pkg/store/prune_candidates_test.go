package store

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestPruneDeletionMarkersSkipsLiveRunLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := tmpStore(t)
		if _, err := s.CreateRun(t.Context(), "a-active", "wf", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRun(t.Context(), "z-deleted", "wf", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRun(t.Context(), "z-deleted"); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(s.root, "runs", "z-deleted", deletionMarkerName)
		if err := os.Chtimes(marker, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
		held, err := s.acquireRunWriteLock("a-active")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = held.Unlock() }()
		n, err := s.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour))
		t.Logf("live run has no tombstone; unrelated old tombstone pruned=%d err=%v", n, err)
		if err != nil || n != 1 {
			t.Fatal("PRUNE BLOCKED BY A LIVE RUN THAT IS NOT A DELETION CANDIDATE")
		}
		if _, err := s.LoadRun(t.Context(), "a-active"); err != nil {
			t.Fatalf("live run changed: %v", err)
		}
	})
}
