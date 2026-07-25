//go:build linux

package runtime

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCleanupRecoveredWorktreeBoundsGroupWritableCheckout(t *testing.T) {
	repo, expectedHEAD := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	if err := os.Chmod(wt, 0o775); err != nil {
		t.Fatalf("chmod group-writable worktree: %v", err)
	}

	censusCalls := 0
	_, err := cleanupRecoveredWorktreeForRun(
		"run-group-writable",
		repo,
		wt,
		"",
		expectedHEAD,
		testWorktreeAuthority(),
		&worktreeCleanupTestHooks{
			processReferences: func(root string, _ time.Time) ([]int, error) {
				censusCalls++
				info, err := os.Stat(root)
				if err != nil {
					return nil, err
				}
				if got := info.Mode().Perm(); got&0o022 != 0 {
					t.Fatalf("recovery mode during census=%#o, want group/other write bits revoked", got)
				}
				return nil, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("cleanup group-writable worktree: %v", err)
	}
	if censusCalls != 2 {
		t.Fatalf("process census calls=%d, want two-snapshot quiescence proof", censusCalls)
	}
	if _, err := os.Lstat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("group-writable worktree still exists after quiescent cleanup: %v", err)
	}
}

func TestCleanupRecoveredWorktreeRetainsWritableMemoryMapping(t *testing.T) {
	repo, expectedHEAD := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")

	writer := exec.Command(os.Args[0], "-test.run=^TestWorktreeMappedWriterHelper$")
	writer.Env = append(
		os.Environ(),
		"GO_WANT_WORKTREE_MAPPED_WRITER=1",
		"WORKTREE_MAPPED_WRITER_OUTSIDE="+t.TempDir(),
	)
	writer.Dir = wt
	writerInput, err := writer.StdinPipe()
	if err != nil {
		t.Fatalf("mapped writer stdin: %v", err)
	}
	writerOutput, err := writer.StdoutPipe()
	if err != nil {
		t.Fatalf("mapped writer stdout: %v", err)
	}
	if err := writer.Start(); err != nil {
		t.Fatalf("start mapped writer: %v", err)
	}
	writerWaited := false
	t.Cleanup(func() {
		if writerWaited {
			return
		}
		_ = writer.Process.Kill()
		_ = writer.Wait()
	})
	var ready [1]byte
	if _, err := io.ReadFull(writerOutput, ready[:]); err != nil || ready[0] != 'r' {
		t.Fatalf("mapped writer readiness = %q, err=%v", ready, err)
	}

	result, err := cleanupRecoveredWorktreeForRun(
		"run-mapped-writer",
		repo,
		wt,
		"",
		expectedHEAD,
		testWorktreeAuthority(),
		nil,
	)
	if err != nil {
		t.Fatalf("quarantine with mapped writer: %v", err)
	}
	if result.RecoveryPath == "" {
		t.Fatalf("mapped writer did not retain recovery worktree: %+v", result)
	}
	if !strings.Contains(result.RetentionReason, "live process references") {
		t.Fatalf("retention reason=%q, want live mapping proof", result.RetentionReason)
	}

	if _, err := writerInput.Write([]byte{'x'}); err != nil {
		t.Fatalf("release mapped writer: %v", err)
	}
	if err := writerInput.Close(); err != nil {
		t.Fatalf("close mapped writer input: %v", err)
	}
	if err := writer.Wait(); err != nil {
		t.Fatalf("mapped writer: %v", err)
	}
	writerWaited = true

	got, err := os.ReadFile(filepath.Join(result.RecoveryPath, "README.md"))
	if err != nil {
		t.Fatalf("read mapped output from recovery: %v", err)
	}
	if len(got) == 0 || got[0] != 'X' {
		t.Fatalf("mapped output was not preserved: %q", got)
	}
}

func TestCleanupRecoveredWorktreeRetainsNonDumpableWriter(t *testing.T) {
	repo, expectedHEAD := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")

	authoritySince := time.Now().UTC()
	writer := exec.Command(os.Args[0], "-test.run=^TestWorktreeNonDumpableWriterHelper$")
	writer.Env = append(os.Environ(), "GO_WANT_WORKTREE_NONDUMPABLE_WRITER=1")
	writer.Dir = wt
	writerInput, err := writer.StdinPipe()
	if err != nil {
		t.Fatalf("non-dumpable writer stdin: %v", err)
	}
	writerOutput, err := writer.StdoutPipe()
	if err != nil {
		t.Fatalf("non-dumpable writer stdout: %v", err)
	}
	if err := writer.Start(); err != nil {
		t.Fatalf("start non-dumpable writer: %v", err)
	}
	writerWaited := false
	t.Cleanup(func() {
		if writerWaited {
			return
		}
		_ = writer.Process.Kill()
		_ = writer.Wait()
	})
	var ready [1]byte
	if _, err := io.ReadFull(writerOutput, ready[:]); err != nil || ready[0] != 'r' {
		t.Fatalf("non-dumpable writer readiness = %q, err=%v", ready, err)
	}
	startedAt, err := procProcessStartedAt(writer.Process.Pid)
	if err != nil {
		t.Fatalf("read non-dumpable writer start time: %v", err)
	}
	if startedAt.Before(authoritySince.Add(-worktreeProcessStartSkew)) {
		t.Fatalf(
			"non-dumpable writer start %s falls outside captured authority %s (skew %s)",
			startedAt.Format(time.RFC3339Nano),
			authoritySince.Format(time.RFC3339Nano),
			worktreeProcessStartSkew,
		)
	}
	// The writer-controlled checkout metadata is not an authority boundary.
	// Advancing .git into the future must not make this live process look older
	// than the worktree.
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(filepath.Join(wt, ".git"), future, future); err != nil {
		t.Fatalf("advance writer-controlled .git mtime: %v", err)
	}

	result, cleanupErr := cleanupRecoveredWorktreeForRun(
		"run-nondumpable-writer",
		repo,
		wt,
		"",
		expectedHEAD,
		authoritySince,
		nil,
	)
	if cleanupErr != nil && result.RecoveryPath == "" {
		t.Fatalf("quarantine non-dumpable writer before recovery reservation: %v", cleanupErr)
	}
	if result.RecoveryPath == "" {
		t.Fatalf("non-dumpable writer did not retain recovery worktree: %+v", result)
	}

	if _, err := writerInput.Write([]byte{'x'}); err != nil {
		t.Fatalf("release non-dumpable writer: %v", err)
	}
	if err := writerInput.Close(); err != nil {
		t.Fatalf("close non-dumpable writer input: %v", err)
	}
	if err := writer.Wait(); err != nil {
		t.Fatalf("non-dumpable writer: %v", err)
	}
	writerWaited = true

	output := filepath.Join(result.RecoveryPath, "nondumpable-late-output")
	if got, err := os.ReadFile(output); err != nil {
		t.Fatalf("non-dumpable writer output was not preserved: %v", err)
	} else if string(got) != "kept" {
		t.Fatalf("non-dumpable writer output=%q", got)
	}
}

func TestWorktreeMappedWriterHelper(t *testing.T) {
	if os.Getenv("GO_WANT_WORKTREE_MAPPED_WRITER") != "1" {
		return
	}
	f, err := os.OpenFile("README.md", os.O_RDWR, 0)
	if err != nil {
		os.Exit(2)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		os.Exit(3)
	}
	mapped, err := unix.Mmap(
		int(f.Fd()),
		0,
		int(info.Size()),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED,
	)
	_ = f.Close()
	if err != nil {
		os.Exit(4)
	}
	defer unix.Munmap(mapped)
	if err := os.Chdir(os.Getenv("WORKTREE_MAPPED_WRITER_OUTSIDE")); err != nil {
		os.Exit(5)
	}
	if _, err := os.Stdout.Write([]byte{'r'}); err != nil {
		os.Exit(6)
	}
	var release [1]byte
	if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
		os.Exit(7)
	}
	mapped[0] = 'X'
	if err := unix.Msync(mapped, unix.MS_SYNC); err != nil {
		os.Exit(8)
	}
}

func TestWorktreeNonDumpableWriterHelper(t *testing.T) {
	if os.Getenv("GO_WANT_WORKTREE_NONDUMPABLE_WRITER") != "1" {
		return
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		os.Exit(2)
	}
	if _, err := os.Stdout.Write([]byte{'r'}); err != nil {
		os.Exit(3)
	}
	var release [1]byte
	if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile("nondumpable-late-output", []byte("kept"), 0o644); err != nil {
		os.Exit(5)
	}
}
