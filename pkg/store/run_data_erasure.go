package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// visitRunDataDirectories keeps each ancestor open while visiting its children.
// Links are refused explicitly: skipping one would report an incomplete erasure
// as complete. Non-directory housekeeping files are not namespace roots.
func visitRunDataDirectories(ctx context.Context, parent *os.Root, visit func(string, *os.Root) error) error {
	d := &runPayloadDirectory{parent: &runPayloadFile{root: parent, name: ".", strict: true}}
	entries, err := d.ReadDir()
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			errs = append(errs, fmt.Errorf("namespace %s is a symlink: %w", entry.Name(), ErrRunDataIdentityChanged))
			continue
		}
		if !entry.IsDir() {
			continue
		}
		child, e := openIncarnationChildRoot(parent, entry.Name(), false)
		if e != nil {
			if !errors.Is(e, os.ErrNotExist) {
				errs = append(errs, e)
			}
			continue
		}
		e = errors.Join(visit(entry.Name(), child), child.Close())
		if e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

func (s *FilesystemRunStore) reattributeIncarnationRunNotes(ctx context.Context, fromAuthor, toAuthor string) (int64, error) {
	// No protocol-one roots is the ordinary historical case, including platforms
	// that cannot create a protocol-one birth. Header presence is not an index:
	// retired and orphaned namespaces remain subjects of global erasure.
	if _, err := os.Lstat(filepath.Join(s.root, ".run-data-v1")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	root, err := openRunDataStoreRoot(s.root)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	spaces, err := openIncarnationChildRoot(root, ".run-data-v1", false)
	if err != nil {
		return 0, err
	}
	defer spaces.Close()
	var total int64
	err = visitRunDataDirectories(ctx, spaces, func(formatName string, formats *os.Root) error {
		n, e := strconv.Atoi(strings.TrimPrefix(formatName, "f"))
		if e != nil || formatName != fmt.Sprintf("f%d", n) {
			return fmt.Errorf("invalid run data format directory %q", formatName)
		}
		format, e := NormalizeValueFormat(n)
		if e != nil {
			return e
		}
		if int(format) != n {
			return fmt.Errorf("noncanonical run data format directory %q: %w", formatName, ErrInvalidRunDataIdentity)
		}
		return visitRunDataDirectories(ctx, formats, func(runID string, runs *os.Root) error {
			if e := sanitizePathComponent("run ID", runID); e != nil {
				return e
			}
			lock, e := s.acquireRunWriteLock(runID)
			if e != nil {
				return e
			}
			defer func() { _ = lock.Unlock() }()
			return visitRunDataDirectories(ctx, runs, func(birthID string, birth *os.Root) error {
				if _, e := CaptureRunDataBinding(RunDataHeader{RunID: runID, BirthID: birthID, Format: format, Protocol: IncarnationRunData}); e != nil {
					return e
				}
				notes, e := openIncarnationChildRoot(birth, "notes", false)
				if e != nil {
					if errors.Is(e, os.ErrNotExist) {
						return nil
					}
					return e
				}
				dir := &runPayloadDirectory{parent: &runPayloadFile{key: filepath.Join(formatName, runID, birthID, "notes"), root: notes, name: ".", strict: true}}
				defer dir.Close()
				seqs, e := noteSeqsDirectory(dir)
				if e != nil {
					return e
				}
				var errs []error
				for _, seq := range seqs {
					if e := ctx.Err(); e != nil {
						return errors.Join(append(errs, e)...)
					}
					note, e := readNoteFileDirectory(dir, seq)
					if e != nil {
						errs = append(errs, fmt.Errorf("namespace %s note %d: %w", dir.parent.key, seq, e))
						continue
					}
					if !NoteAuthorMatches(note.Author, fromAuthor) {
						continue
					}
					note.Author = toAuthor
					data, e := json.MarshalIndent(note, "", "  ")
					if e != nil {
						errs = append(errs, e)
						continue
					}
					if e = dir.WriteAtomic(ctx, fmt.Sprintf("%04d.json", seq), data, filePerm); e != nil {
						errs = append(errs, e)
						continue
					}
					total++
				}
				return errors.Join(errs...)
			})
		})
	})
	return total, err
}
