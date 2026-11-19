package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bindFilesystemRun binds ctx to the observation of the run id holds now.
func bindFilesystemRun(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string) context.Context {
	t.Helper()
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	o, err := CaptureRunObservation(RunDataHeader{RunID: r.ID, Format: ValueFormat(r.FormatVersion), Protocol: r.DataProtocol, BirthID: r.BirthID}, r.TenantID, true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindRunObservation(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// TestBoundDeleteAnswersWhatABoundLoadAnswers: a delete bound to a run that
// was deleted answers ErrRunDeleted while its tombstone stands and
// ErrRunNotFound once the tombstone is reaped — never a silent idempotent
// success that would report the bound run removed by this call.
func TestBoundDeleteAnswersWhatABoundLoadAnswers(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := s.CreateRun(ctx, "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	bound := bindFilesystemRun(t, s, ctx, "r")
	if err := s.DeleteRun(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.runDir("r"), deletionMarkerName)
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(bound, "r"); !errors.Is(err, ErrRunDeleted) {
		t.Fatalf("bound delete of a tombstoned run = %v, want ErrRunDeleted", err)
	}
	if after, err := os.ReadFile(marker); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused bound delete rewrote the tombstone: %v", err)
	}
	if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("PruneDeletionMarkers = (%d, %v), want (1, nil)", n, err)
	}
	if err := s.DeleteRun(bound, "r"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("bound delete of a reaped run = %v, want ErrRunNotFound", err)
	}
	if _, err := os.Stat(s.runDir("r")); !os.IsNotExist(err) {
		t.Fatalf("a refused bound delete left a run directory behind: %v", err)
	}
}

// TestUnboundDeletionKeepsTheFormatOnlyHeaderCheck: a run.json the strict
// header parser refuses (a birth_id case alias) while the format read
// accepts it stays deletable and prunable by unbound callers, as before the
// bound door; a bound delete reads the header and refuses it, the document
// untouched.
func TestUnboundDeletionKeepsTheFormatOnlyHeaderCheck(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"delete", "prune", "bound-delete"} {
		t.Run(op, func(t *testing.T) {
			s, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if _, err := s.CreateRun(ctx, "r", "wf", nil); err != nil {
				t.Fatal(err)
			}
			bound := bindFilesystemRun(t, s, ctx, "r")
			raw, err := os.ReadFile(s.runJSONPath("r"))
			if err != nil {
				t.Fatal(err)
			}
			legacy := []byte(strings.Replace(string(raw), "{", `{"Birth_Id": "x",`, 1))
			if err := os.WriteFile(s.runJSONPath("r"), legacy, filePerm); err != nil {
				t.Fatal(err)
			}
			if _, err := RunValueFormatFromJSON(ctx, legacy); err != nil {
				t.Fatalf("fixture: the format read refuses the legacy shape: %v", err)
			}
			if _, err := RunDataHeaderFromJSON(ctx, legacy); !errors.Is(err, ErrInvalidRunDataIdentity) {
				t.Fatalf("fixture: the header parser accepts the legacy shape: %v", err)
			}
			switch op {
			case "delete":
				if err := s.DeleteRun(ctx, "r"); err != nil {
					t.Fatalf("unbound delete of the legacy shape = %v, want nil", err)
				}
				if _, err := os.Stat(s.runJSONPath("r")); !os.IsNotExist(err) {
					t.Fatalf("unbound delete kept run.json: %v", err)
				}
				if !s.runDeleted("r") {
					t.Fatal("unbound delete wrote no tombstone")
				}
			case "prune":
				if err := os.WriteFile(filepath.Join(s.runDir("r"), deletionMarkerName), []byte("interrupted"), filePerm); err != nil {
					t.Fatal(err)
				}
				if n, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
					t.Fatalf("unbound prune of a tombstone retaining the legacy shape = (%d, %v), want (1, nil)", n, err)
				}
				if _, err := os.Stat(s.runDir("r")); !os.IsNotExist(err) {
					t.Fatalf("unbound prune kept the run directory: %v", err)
				}
			case "bound-delete":
				if err := s.DeleteRun(bound, "r"); !errors.Is(err, ErrInvalidRunDataIdentity) {
					t.Fatalf("bound delete of the legacy shape = %v, want ErrInvalidRunDataIdentity", err)
				}
				if after, err := os.ReadFile(s.runJSONPath("r")); err != nil || !bytes.Equal(after, legacy) {
					t.Fatalf("a refused bound delete changed run.json: %v", err)
				}
				if s.runDeleted("r") {
					t.Fatal("a refused bound delete wrote a tombstone")
				}
			}
		})
	}
}
