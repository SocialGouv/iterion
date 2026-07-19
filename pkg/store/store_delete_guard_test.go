package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTombstoneGuardNewlyGuardedWriters proves the write paths that
// (re)create files under runs/<id>/ WITHOUT flowing through writeRun —
// CreateQueuedRun's exclusive create, the artifact / tool-blob sidecars,
// and the run-files scratch dir — all refuse a tombstoned run with the
// typed ErrRunDeleted, and leave the tombstoned directory untouched
// (deletion marker only). Complements the shared conformance suite,
// which covers CreateRun / SaveRun / AppendEvent / AppendQueuedMessage.
func TestTombstoneGuardNewlyGuardedWriters(t *testing.T) {
	ctx := context.Background()

	ops := []struct {
		name string
		op   func(s *FilesystemRunStore, runID string) error
	}{
		{"CreateQueuedRun", func(s *FilesystemRunStore, id string) error {
			_, err := s.CreateQueuedRun(ctx, id, "zombie", "/x.bot", "bot", nil)
			return err
		}},
		{"WriteArtifact", func(s *FilesystemRunStore, id string) error {
			return s.WriteArtifact(ctx, &Artifact{RunID: id, NodeID: "n", Version: 0, Data: map[string]any{"k": "v"}})
		}},
		{"WriteToolBlob", func(s *FilesystemRunStore, id string) error {
			_, err := s.WriteToolBlob(ctx, id, "tu_1", "output", []byte("late"))
			return err
		}},
		{"EnsureRunFilesDir", func(s *FilesystemRunStore, id string) error {
			_, err := s.EnsureRunFilesDir(ctx, id)
			return err
		}},
	}
	for _, tc := range ops {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			mustCreateRun(t, s, "run-tomb")
			if err := s.DeleteRun(ctx, "run-tomb"); err != nil {
				t.Fatalf("DeleteRun: %v", err)
			}
			if err := tc.op(s, "run-tomb"); !errors.Is(err, ErrRunDeleted) {
				t.Fatalf("%s on tombstoned run: err = %v, want ErrRunDeleted", tc.name, err)
			}
			// The refusal happened BEFORE any write: the tombstoned dir
			// still holds exactly the deletion marker, nothing else.
			entries, err := os.ReadDir(filepath.Join(s.root, "runs", "run-tomb"))
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != deletionMarkerName {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("%s rebuilt the tombstoned run dir: %v", tc.name, names)
			}
		})
	}
}

// TestWriteArtifactBeforeRunJSON proves the index-update skip for a run
// whose run.json does not exist yet (early CreateRun race): the wrapped
// ErrRunNotFound from loadRunRaw is matched via errors.Is, the artifact
// lands on disk, and it is readable through the directory-scan fallback.
func TestWriteArtifactBeforeRunJSON(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	a := &Artifact{RunID: "run-early", NodeID: "n", Version: 0, Data: map[string]any{"k": "v"}}
	if err := s.WriteArtifact(ctx, a); err != nil {
		t.Fatalf("WriteArtifact without run.json: %v", err)
	}
	got, err := s.LoadLatestArtifact(ctx, "run-early", "n")
	if err != nil {
		t.Fatalf("LoadLatestArtifact: %v", err)
	}
	if got.Data["k"] != "v" {
		t.Errorf("artifact data = %v, want the persisted payload", got.Data)
	}
}

// TestWriteArtifactIndexLoadFailureSurfaces proves a run.json that exists
// but cannot be decoded fails WriteArtifact loudly — the absence check
// (errors.Is(ErrRunNotFound)) must not swallow a real load failure.
func TestWriteArtifactIndexLoadFailureSurfaces(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	mustCreateRun(t, s, "run-corrupt")
	if err := os.WriteFile(s.runJSONPath("run-corrupt"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt run.json: %v", err)
	}
	err := s.WriteArtifact(ctx, &Artifact{RunID: "run-corrupt", NodeID: "n", Version: 0})
	if err == nil {
		t.Fatal("WriteArtifact with corrupt run.json: want error, got nil")
	}
	if errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrRunDeleted) {
		t.Fatalf("decode failure misclassified as absence/deletion: %v", err)
	}
}
