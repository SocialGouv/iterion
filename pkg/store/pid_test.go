package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestPIDFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.CreateRun(context.Background(), "run-pid", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Initially absent.
	if pid, err := s.ReadPIDFile("run-pid"); err != nil || pid != 0 {
		t.Fatalf("ReadPIDFile (absent) = (%d, %v), want (0, nil)", pid, err)
	}

	if err := s.WritePIDFile("run-pid", 12345); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	pid, err := s.ReadPIDFile("run-pid")
	if err != nil || pid != 12345 {
		t.Fatalf("ReadPIDFile = (%d, %v), want (12345, nil)", pid, err)
	}

	if err := s.RemovePIDFile("run-pid"); err != nil {
		t.Fatalf("RemovePIDFile: %v", err)
	}
	if pid, err := s.ReadPIDFile("run-pid"); err != nil || pid != 0 {
		t.Errorf("ReadPIDFile after remove = (%d, %v), want (0, nil)", pid, err)
	}
	// Idempotent.
	if err := s.RemovePIDFile("run-pid"); err != nil {
		t.Errorf("RemovePIDFile (already gone): %v, want nil", err)
	}
}

func TestPIDFile_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []string{"../foo", "foo/bar", "..", ""}
	for _, bad := range cases {
		if err := s.WritePIDFile(bad, 1); err == nil {
			t.Errorf("WritePIDFile(%q) = nil, want error", bad)
		}
	}
}

func TestPIDFile_RejectsMalformedContent(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.CreateRun(context.Background(), "run-bad", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Manually plant malformed content under the pid path so we test
	// the read-side parser, not the write-side validator.
	pidPath := s.PIDFilePath("run-bad")
	if err := os.WriteFile(pidPath, []byte("not-a-number\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err = s.ReadPIDFile("run-bad")
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("ReadPIDFile (malformed) = %v, want parse error", err)
	}

	if err := os.WriteFile(pidPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err = s.ReadPIDFile("run-bad")
	if err == nil {
		t.Errorf("ReadPIDFile (zero pid) = nil, want error")
	}
}

func TestPIDFile_AtomicReplace(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.CreateRun(context.Background(), "run-atom", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := s.WritePIDFile("run-atom", 1); err != nil {
		t.Fatalf("WritePIDFile #1: %v", err)
	}
	if err := s.WritePIDFile("run-atom", 2); err != nil {
		t.Fatalf("WritePIDFile #2: %v", err)
	}
	pid, err := s.ReadPIDFile("run-atom")
	if err != nil || pid != 2 {
		t.Errorf("ReadPIDFile after replace = (%d, %v), want (2, nil)", pid, err)
	}

	// No leftover .pid.tmp-* files (test the atomic rename's cleanup).
	entries, err := os.ReadDir(dir + "/runs/run-atom")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pid.tmp-") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestPIDFile_MissingRunDir(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// WritePIDFile is supposed to MkdirAll, so it should succeed even
	// when no CreateRun has been called yet (this is a defensive
	// property — a pre-creation race shouldn't lose .pid bookkeeping).
	if err := s.WritePIDFile("run-implicit", 42); err != nil {
		t.Fatalf("WritePIDFile (implicit dir): %v", err)
	}
	pid, err := s.ReadPIDFile("run-implicit")
	if err != nil || pid != 42 {
		t.Fatalf("ReadPIDFile = (%d, %v), want (42, nil)", pid, err)
	}
}

func TestPIDFile_ReadIgnoresWhitespace(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.CreateRun(context.Background(), "run-ws", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	pidPath := s.PIDFilePath("run-ws")
	if err := os.WriteFile(pidPath, []byte("  101  \n\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	pid, err := s.ReadPIDFile("run-ws")
	if err != nil || pid != 101 {
		t.Errorf("ReadPIDFile (whitespace) = (%d, %v), want (101, nil)", pid, err)
	}
}

// Ensure ReadPIDFile distinguishes "absent" (no error) from "open
// failed" (error). A stale .pid pointing at a now-removed dir would
// produce ErrNotExist; we report that as (0, nil) just like a never-
// written file.
func TestPIDFile_AbsentNotAnError(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pid, err := s.ReadPIDFile("never-existed")
	if err != nil {
		// Defensive cross-check via os.IsNotExist in case the impl
		// regresses to wrapping the error.
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("ReadPIDFile leaked ErrNotExist; should report (0, nil) for missing run")
		} else {
			t.Errorf("ReadPIDFile (absent) = %v, want nil error", err)
		}
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0", pid)
	}
}
