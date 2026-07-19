package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// WriteFileAtomic writes data to path atomically by first writing to a sibling
// temp file (path+".tmp"), fsyncing, and then renaming over the destination.
// On POSIX, rename(2) is atomic for paths on the same filesystem, so a reader
// observes either the prior contents or the new contents — never a torn write.
//
// This matters for run.json (the authoritative resume checkpoint per CLAUDE.md):
// the prior code path used os.WriteFile, which truncates and then writes; a
// SIGKILL/OOM/power-loss between truncate and write produced an empty or
// partial JSON that LoadRun could no longer decode, making the run permanently
// unresumable.
//
// On error, the temp file is best-effort removed so we don't leak it.
//
// Exported so other on-disk subsystems (e.g. the privacy vault) can reuse
// the same write semantics without duplicating the algorithm.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	// Use os.CreateTemp for a unique temp name. The previous fixed
	// `path+".tmp"` collided when two concurrent writers (e.g. an
	// in-process write racing an external process touching the same
	// file) both staged through the same temp path, producing torn
	// renames. CreateTemp + chmod gives us per-call isolation.
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".atomic-*")
	if err != nil {
		return fmt.Errorf("store: open temp file: %w", err)
	}
	tmp := f.Name()
	// Apply the requested mode now; CreateTemp uses 0600.
	if err := os.Chmod(tmp, perm); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: chmod temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: write temp file: %w", err)
	}
	// fsync the file contents before rename so the new bytes are durably on
	// disk; otherwise a crash after rename but before the data block flush
	// could still surface a zero-length file on recovery.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: rename temp file: %w", err)
	}
	// fsync the parent directory so the rename itself is durably on
	// disk. Without this, ext4 (and other journalling filesystems) can
	// commit the file data and the rename out-of-order on crash, leaving
	// the prior name pointing nowhere — the run.json we just "atomically"
	// replaced can vanish after a power loss. fsync-on-dir is the
	// well-known second half of a durable atomic write; missing it was
	// the long-standing footgun.
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("store: sync dir: %w", err)
	}
	return nil
}

// WriteFileAtomicNew writes data to path durably but refuses to clobber an
// existing destination. It stages through a sibling temp file, fsyncs the
// bytes, then atomically links the temp inode into place. The hard-link step is
// an exclusive create: if path already exists (or a racing creator wins), the
// destination is left untouched and fs.ErrExist is returned in the error chain.
func WriteFileAtomicNew(path string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".create-*")
	if err != nil {
		return fmt.Errorf("store: open temp file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Chmod(tmp, perm); err != nil {
		f.Close()
		return fmt.Errorf("store: chmod temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("store: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("store: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close temp file: %w", err)
	}
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("store: create %s: %w", path, fs.ErrExist)
		}
		return fmt.Errorf("store: link temp file: %w", err)
	}
	// Drop the temp directory entry now that path links to the fsynced inode,
	// then sync the directory so both the new run.json name and temp cleanup are
	// durable. The deferred Remove is harmless if this succeeds.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: remove temp file: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("store: sync dir: %w", err)
	}
	return nil
}

// fsyncDir opens dir and calls Sync on the descriptor so the directory
// entry update from the most recent rename/create reaches the platter.
// On platforms where opening a directory for sync isn't supported the
// fallback is a best-effort no-op (see openat_other.go's companions).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		// On some platforms (notably Windows) Sync on a directory
		// handle returns EBADF/ENOTSUP. Swallow those — the atomic
		// rename semantics differ on those filesystems anyway.
		if errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP) {
			return closeErr
		}
		return syncErr
	}
	return closeErr
}

// SanitizePathComponent validates that a path component (RunID, NodeID,
// InteractionID) does not contain path traversal sequences, separators,
// null bytes, or control characters. Used at every store/runview/blob
// entry point that path-joins user-derived IDs into the run directory,
// and also at the HMAC-signing entry points where control characters in
// a component could collide with a separator in the MAC plaintext (see
// PresignAttachment) and make one signature accidentally valid for a
// different (runID, name) pair.
func SanitizePathComponent(name, component string) error {
	if component == "" {
		return fmt.Errorf("store: %s must not be empty", name)
	}
	if strings.Contains(component, "..") {
		return fmt.Errorf("store: %s %q contains path traversal", name, component)
	}
	if strings.ContainsAny(component, "/\\") {
		return fmt.Errorf("store: %s %q contains path separator", name, component)
	}
	for _, r := range component {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("store: %s %q contains control character", name, component)
		}
	}
	return nil
}
