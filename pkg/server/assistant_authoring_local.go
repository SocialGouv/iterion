package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

// A lock serializes the server's participating writers across processes and
// workspace aliases. Rename publication additionally preserves displaced inodes:
// an external editor may still hold a writable descriptor after we return.
// Locks and recovery objects are persistent; deleting either loses a guarantee.
type authoringLocalLock struct {
	path            string
	parent, control *authoringDirectory
	controlAlias    string
	flock           *flock.Flock
	lockInfo        os.FileInfo
	lockName        string
}

type authoringDirectory struct {
	path string
	info os.FileInfo
	file *os.File
	root *os.Root
}

func openAuthoringDirectory(path string) (*authoringDirectory, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("unsafe authoring directory %s", path)
	}
	file, err := openAuthoringDirectoryFile(path, info)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	d := &authoringDirectory{path: path, info: info, file: file, root: root}
	pinned, err := root.Stat(".")
	if err != nil || !os.SameFile(info, pinned) {
		d.close()
		return nil, errors.New("authoring directory changed while opening")
	}
	return d, nil
}
func (d *authoringDirectory) close() { _ = d.root.Close(); _ = d.file.Close() }
func (d *authoringDirectory) verify() error {
	info, err := os.Lstat(d.path)
	if err != nil {
		return err
	}
	if !os.SameFile(d.info, info) || !info.IsDir() || info.Mode() != d.info.Mode() {
		return authoringConflictError{"authoring parent directory changed: " + d.path}
	}
	return nil
}
func (d *authoringDirectory) read(name string, limit int64) ([]byte, os.FileInfo, error) {
	info, err := d.root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, info, errors.New("authoring file is not regular")
	}
	f, err := openAuthoringFileAt(d.file, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	pinned, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(info, pinned) || !pinned.Mode().IsRegular() {
		return nil, pinned, authoringConflictError{"authoring file changed while opening"}
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("authoring file exceeds the read limit")
	}
	return b, pinned, err
}

var authoringControlName = regexp.MustCompile(`^(?:[a-f0-9]{64}\.lock|[a-f0-9-]{36}\.(?:record|candidate|before|undo))$`)

func newAuthoringLocalLock(ctx context.Context, path string) (*authoringLocalLock, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	physical, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	parent, err := openAuthoringDirectory(physical)
	if err != nil {
		return nil, err
	}
	l := &authoringLocalLock{path: abs, parent: parent}
	ok := false
	defer func() {
		if !ok {
			l.close()
		}
	}()
	if err := mkdirAuthoringAt(parent.file, ".iterion"); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	// A concurrent creator may not have flushed this directory entry yet.
	if err := syncAuthoringDirectory(parent.file); err != nil {
		return nil, err
	}
	// An operator-owned .iterion symlink may point to another disk. It is a
	// valid lock location for in-place saves; replacement preflights EXDEV.
	iterionPath, err := filepath.EvalSymlinks(filepath.Join(physical, ".iterion"))
	if err != nil {
		return nil, err
	}
	iterion, err := openAuthoringDirectory(iterionPath)
	if err != nil {
		return nil, err
	}
	defer iterion.close()
	if err := validateAuthoringOwnership(iterion.file, false); err != nil {
		return nil, err
	}
	if err := mkdirAuthoringAt(iterion.file, "authoring"); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	if err := syncAuthoringDirectory(iterion.file); err != nil {
		return nil, err
	}
	if err := iterion.verify(); err != nil {
		return nil, err
	}
	l.controlAlias = filepath.Join(physical, ".iterion", "authoring")
	control, err := openAuthoringDirectory(filepath.Join(iterionPath, "authoring"))
	if err != nil {
		return nil, err
	}
	l.control = control
	if err := validateAuthoringOwnership(control.file, true); err != nil {
		return nil, err
	}
	if err := l.verify(); err != nil {
		return nil, err
	}
	// Exclusion precedes lock, candidate, journal and recovery data. Never
	// rewrite an existing user ignore file or silently accept a tracked layout.
	ignore, err := openAuthoringFileAt(control.file, ".gitignore", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, writeErr := ignore.WriteString("*\n")
		err = errors.Join(writeErr, ignore.Sync(), ignore.Close(), syncAuthoringDirectory(control.file))
	} else if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	body, err := readAuthoringIgnore(ctx, control)
	if err != nil || string(body) != "*\n" {
		return nil, fmt.Errorf("unsafe authoring ignore file in %s: %w", control.path, errors.Join(err, errors.New("expected private * exclusion")))
	}
	if err := validateAuthoringGitExclusion(ctx, control.path); err != nil {
		return nil, err
	}
	entries, err := control.root.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := entries.Readdirnames(-1)
	err = errors.Join(err, entries.Close())
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if name != ".gitignore" && !authoringControlName.MatchString(name) {
			return nil, fmt.Errorf("unexpected authoring control entry: %s", filepath.Join(control.path, name))
		}
		// Displaced objects are retained even when an external writer replaced
		// the live path with a symlink or another non-regular object. Never follow
		// those objects. All other private files must remain owned regular files.
		if strings.HasSuffix(name, ".before") || strings.HasSuffix(name, ".undo") {
			continue
		}
		info, err := control.root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unsafe authoring control file: %s", name)
		}
		if name == ".gitignore" || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".record") {
			f, err := openAuthoringFileAt(control.file, name, os.O_RDONLY, 0)
			if err != nil {
				return nil, err
			}
			err = errors.Join(validateAuthoringOwnership(f, true), f.Close())
			if err != nil {
				return nil, err
			}
		}
	}
	// Conservative folding also serializes differently cased spellings on
	// case-insensitive filesystems. Extra contention on sensitive filesystems
	// is harmless. NFC coalesces Darwin's composed/decomposed name aliases.
	base := filepath.Base(abs)
	if canonical, err := filepath.EvalSymlinks(abs); err == nil {
		canonicalParent, err := os.Stat(filepath.Dir(canonical))
		if err != nil {
			return nil, err
		}
		if !os.SameFile(parent.info, canonicalParent) {
			return nil, authoringConflictError{"authoring destination alias changed its parent"}
		}
		base = filepath.Base(canonical)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	name := strings.ToLower(norm.NFC.String(normalizeAuthoringLockName(base)))
	l.lockName = contentSHA256(name) + ".lock"
	f, err := openAuthoringFileAt(control.file, l.lockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	l.lockInfo, err = f.Stat()
	if err == nil {
		if !l.lockInfo.Mode().IsRegular() {
			err = errors.New("authoring lock is not a regular file")
		} else {
			err = validateAuthoringOwnership(f, true)
		}
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	l.flock = flock.New(filepath.Join(control.path, l.lockName), flock.SetFlag(authoringLockOpenFlags()), flock.SetPermissions(0o600))
	ok = true
	return l, nil
}

// An exclusive creator can be preempted between creating and writing the two
// ignore bytes. Wait briefly only for that empty state; never rewrite a rule.
func readAuthoringIgnore(ctx context.Context, control *authoringDirectory) ([]byte, error) {
	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		// Read with room to spare: a file that is not the two bytes `*\n`
		// is refused for what it holds, not for its length.
		body, _, err := control.read(".gitignore", 64)
		if err != nil || len(body) != 0 {
			return body, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("authoring ignore file remained empty")
		case <-tick.C:
		}
	}
}

func validateAuthoringGitExclusion(ctx context.Context, dir string) error {
	// No Git installation is necessary outside a worktree. Detection uses the
	// filesystem, including linked worktrees whose .git is a file.
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			tracked, err := runAuthoringGit(ctx, dir, "ls-files", "-z", "--", ".")
			if err != nil {
				return fmt.Errorf("verify authoring Git exclusion: %w", err)
			}
			if tracked != "" {
				return errors.New("authoring control directory contains tracked or staged files")
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

func (l *authoringLocalLock) verify() error {
	if err := l.parent.verify(); err != nil {
		return err
	}
	actual, err := os.Stat(filepath.Dir(l.path))
	if err != nil {
		return err
	}
	if !os.SameFile(l.parent.info, actual) {
		return authoringConflictError{"authoring destination parent changed"}
	}
	if l.control != nil {
		if err := l.control.verify(); err != nil {
			return err
		}
		actual, err := os.Stat(l.controlAlias)
		if err != nil {
			return err
		}
		if !os.SameFile(l.control.info, actual) {
			return authoringConflictError{"authoring recovery directory changed"}
		}
	}
	return nil
}
func (l *authoringLocalLock) close() {
	if l.flock != nil {
		_ = l.flock.Close()
	}
	if l.control != nil {
		l.control.close()
	}
	if l.parent != nil {
		l.parent.close()
	}
}
func acquireAuthoringLocalLocks(ctx context.Context, paths []string) ([]*authoringLocalLock, error) {
	locks := make([]*authoringLocalLock, 0, len(paths))
	fail := func(err error) ([]*authoringLocalLock, error) { closeAuthoringLocalLocks(locks); return nil, err }
	seen := map[string]*flock.Flock{}
	for _, path := range paths {
		l, err := newAuthoringLocalLock(ctx, path)
		if err != nil {
			return fail(err)
		}
		key := filepath.Join(l.control.path, l.lockName)
		if held := seen[key]; held != nil {
			// Conservative name folding may coalesce distinct case-sensitive
			// destinations. Share one lock handle instead of rejecting valid
			// files or deadlocking against ourselves.
			l.flock = held
		} else {
			seen[key] = l.flock
		}
		locks = append(locks, l)
	}
	// Stable order prevents deadlock across requests listing files differently.
	ordered := append([]*authoringLocalLock(nil), locks...)
	sort.Slice(ordered, func(i, j int) bool {
		return filepath.Join(ordered[i].control.path, ordered[i].lockName) < filepath.Join(ordered[j].control.path, ordered[j].lockName)
	})
	for _, l := range ordered {
		won, err := l.flock.TryLockContext(ctx, 20*time.Millisecond)
		if err != nil {
			return fail(err)
		}
		if !won {
			return fail(ctx.Err())
		}
		if err := l.verify(); err != nil {
			return fail(err)
		}
		ignore, err := readAuthoringIgnore(ctx, l.control)
		if err != nil {
			return fail(err)
		}
		if string(ignore) != "*\n" {
			return fail(authoringConflictError{"authoring exclusion changed while acquiring lock"})
		}
		if err := validateAuthoringGitExclusion(ctx, l.control.path); err != nil {
			return fail(err)
		}
		info, err := l.control.root.Lstat(l.lockName)
		if err != nil {
			return fail(err)
		}
		lockedInfo, statErr := l.flock.Stat()
		if statErr != nil {
			return fail(statErr)
		}
		if !os.SameFile(l.lockInfo, info) || !os.SameFile(info, lockedInfo) {
			return fail(authoringConflictError{"authoring lock file changed"})
		}
	}
	// The caller receives its original request order.
	return locks, nil
}
func closeAuthoringLocalLocks(locks []*authoringLocalLock) {
	ordered := append([]*authoringLocalLock(nil), locks...)
	sort.Slice(ordered, func(i, j int) bool {
		return filepath.Join(ordered[i].control.path, ordered[i].lockName) > filepath.Join(ordered[j].control.path, ordered[j].lockName)
	})
	for _, l := range ordered {
		l.close()
	}
}

func (l *authoringLocalLock) writeEditorFile(content []byte, createOnly bool, ignore func(string)) error {
	if err := l.verify(); err != nil {
		return err
	}
	if createOnly {
		if _, err := l.parent.root.Lstat(filepath.Base(l.path)); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		tx, err := prepareAuthoringLocal(l, authoringPreviewFile{Operation: "create", Path: l.path, After: string(content)}, ignore, nil)
		if tx == nil {
			return err
		}
		defer tx.close()
		if err == nil {
			err = tx.publish()
		}
		if err != nil {
			return fmt.Errorf("create file: %w; recovery record: %s", errors.Join(err, tx.rollback()), tx.recovery().Record)
		}
		return nil
	}
	if err := l.verify(); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE
	f, err := openAuthoringFileAt(l.parent.file, filepath.Base(l.path), flags, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("authoring destination is not a regular file")
	}
	if err := l.verify(); err != nil {
		return err
	}
	// Preserve the editor's in-place semantics and inode under the shared lock.
	if ignore != nil {
		ignore(l.path)
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return err
	}
	return f.Sync()
}

type authoringRecovery struct {
	Scope  string   `json:"scope"`
	Path   string   `json:"path"`
	Record string   `json:"record"`
	Files  []string `json:"files"`
}

type authoringLocalTransaction struct {
	lock                                *authoringLocalLock
	preview                             authoringPreviewFile
	id                                  string
	journal                             *os.File
	candidateInfo, expectedInfo         os.FileInfo
	displaced, published, undoDisplaced bool
	// Narrow fault/barrier seam: called immediately before each filesystem
	// transition, allowing tests to exercise actual interference at the syscall.
	beforeStep func(string) error
	ignore     func(string)
}

func prepareAuthoringLocal(l *authoringLocalLock, p authoringPreviewFile, ignore func(string), hook func(string) error) (*authoringLocalTransaction, error) {
	tx := &authoringLocalTransaction{lock: l, preview: p, id: uuid.NewString(), ignore: ignore, beforeStep: hook}
	if err := l.verify(); err != nil {
		return nil, err
	}
	if err := requireAuthoringSameFilesystem(l.parent.file, l.control.file); err != nil {
		return nil, err
	}
	mode := os.FileMode(0o644)
	if p.Operation != "create" {
		body, info, err := l.parent.read(filepath.Base(l.path), int64(currentAuthoringLimits().maxTotalBytes))
		if err != nil {
			return nil, err
		}
		if string(body) != p.Before {
			return nil, authoringConflictError{"authoring destination changed before preparation"}
		}
		tx.expectedInfo = info
		mode = info.Mode().Perm()
	}
	journal, err := openAuthoringFileAt(l.control.file, tx.name("record"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	tx.journal = journal
	if err = tx.stage("preparing"); err != nil {
		return tx, err
	}
	if err = tx.step("prepare"); err != nil {
		return tx, err
	}
	candidate, err := openAuthoringFileAt(l.control.file, tx.name("candidate"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return tx, err
	}
	_, writeErr := candidate.WriteString(p.After)
	// Preserve executable/read bits of replacements; the enclosing directory
	// remains private until publication.
	modeErr := candidate.Chmod(mode)
	tx.candidateInfo, err = candidate.Stat()
	err = errors.Join(writeErr, modeErr, err, candidate.Sync(), candidate.Close())
	if err != nil {
		return tx, err
	}
	if err = syncAuthoringDirectory(l.control.file); err != nil {
		return tx, err
	}
	return tx, tx.stage("prepared")
}
func (tx *authoringLocalTransaction) name(kind string) string { return tx.id + "." + kind }
func (tx *authoringLocalTransaction) step(stage string) error {
	if tx.beforeStep != nil {
		if err := tx.beforeStep(stage); err != nil {
			return err
		}
	}
	return tx.lock.verify()
}
func (tx *authoringLocalTransaction) stage(stage string) error {
	if err := tx.step("record:" + stage); err != nil {
		return err
	}
	record := struct {
		Stage           string `json:"stage"`
		Target          string `json:"target"`
		ExpectedSHA256  string `json:"expected_sha256"`
		CandidateSHA256 string `json:"candidate_sha256"`
		Candidate       string `json:"candidate"`
		Original        string `json:"original"`
		Undo            string `json:"undo"`
	}{stage, tx.lock.path, contentSHA256(tx.preview.Before), contentSHA256(tx.preview.After), tx.name("candidate"), tx.name("before"), tx.name("undo")}
	if err := json.NewEncoder(tx.journal).Encode(record); err != nil {
		return err
	}
	return tx.journal.Sync()
}
func (tx *authoringLocalTransaction) move(stage string, from *authoringDirectory, fromName string, to *authoringDirectory, toName string) error {
	if err := tx.step(stage); err != nil {
		return err
	}
	if tx.ignore != nil {
		tx.ignore(tx.lock.path)
	}
	return renameAuthoringAt(from.file, fromName, to.file, toName)
}
func (tx *authoringLocalTransaction) syncDirs() error {
	return errors.Join(syncAuthoringDirectory(tx.lock.parent.file), syncAuthoringDirectory(tx.lock.control.file))
}
func (tx *authoringLocalTransaction) publish() error {
	l := tx.lock
	name := filepath.Base(l.path)
	if tx.preview.Operation != "create" {
		if err := tx.stage("displacing"); err != nil {
			return err
		}
		if err := tx.move("displace", l.parent, name, l.control, tx.name("before")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errors.Join(authoringConflictError{"authoring destination disappeared during commit"}, err)
			}
			return err
		}
		tx.displaced = true
		if err := errors.Join(tx.syncDirs(), tx.stage("displaced")); err != nil {
			return err
		}
		body, info, err := l.control.read(tx.name("before"), int64(currentAuthoringLimits().maxTotalBytes))
		if err != nil || !os.SameFile(tx.expectedInfo, info) || info.Mode() != tx.expectedInfo.Mode() || string(body) != tx.preview.Before {
			conflict := authoringConflictError{"authoring destination changed during commit"}
			restoreErr := tx.restoreOriginal()
			return errors.Join(conflict, err, restoreErr)
		}
	}
	if err := tx.stage("publishing"); err != nil {
		return err
	}
	if err := tx.move("publish", l.control, tx.name("candidate"), l.parent, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			err = errors.Join(authoringConflictError{"another writer created the authoring destination during commit"}, err)
		}
		return err
	}
	tx.published = true
	if err := errors.Join(tx.syncDirs(), tx.stage("published")); err != nil {
		return err
	}
	return l.verify()
}
func (tx *authoringLocalTransaction) restoreOriginal() error {
	if !tx.displaced {
		return nil
	}
	l := tx.lock
	if err := tx.stage("restoring-original"); err != nil {
		return err
	}
	if err := tx.move("restore", l.control, tx.name("before"), l.parent, filepath.Base(l.path)); err != nil {
		return err
	}
	tx.displaced = false
	return errors.Join(tx.syncDirs(), tx.stage("original-restored"))
}
func (tx *authoringLocalTransaction) rollback() error {
	if !tx.published {
		return tx.restoreOriginal()
	}
	l := tx.lock
	name := filepath.Base(l.path)
	if err := tx.stage("rolling-back"); err != nil {
		return err
	}
	if err := tx.move("undo-displace", l.parent, name, l.control, tx.name("undo")); err != nil {
		return err
	}
	tx.undoDisplaced = true
	if err := errors.Join(tx.syncDirs(), tx.stage("undo-displaced")); err != nil {
		return err
	}
	body, info, err := l.control.read(tx.name("undo"), int64(currentAuthoringLimits().maxTotalBytes))
	if err != nil || !os.SameFile(tx.candidateInfo, info) || info.Mode() != tx.candidateInfo.Mode() || string(body) != tx.preview.After {
		// The foreign inode takes precedence, even if it is a symlink or it only
		// happens to contain our candidate bytes. Restore it without overwriting.
		restoreErr := tx.move("undo-restore-foreign", l.control, tx.name("undo"), l.parent, name)
		if restoreErr == nil {
			tx.undoDisplaced = false
		}
		return errors.Join(authoringConflictError{"content changed after write; rollback refused"}, err, restoreErr, tx.syncDirs(), tx.stage("rollback-conflict"))
	}
	if err := tx.restoreOriginal(); err != nil {
		return err
	}
	if tx.preview.Operation == "create" {
		if _, err := l.parent.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(authoringConflictError{"destination reappeared during creation rollback"}, err)
		}
	}
	tx.published = false
	// For creates the live path is now absent. The removed candidate remains
	// retained: an external process could have opened it before rollback.
	return tx.stage("rolled-back")
}

// complete drops the journal and the displaced original of a transaction
// that published: nothing is left to recover. The candidate is the live
// file now; the lock and the exclusion file stay with the directory.
func (tx *authoringLocalTransaction) complete() error {
	if !tx.published {
		return errors.New("authoring: complete called on a transaction that did not publish")
	}
	l := tx.lock
	var errs []error
	if tx.displaced {
		if err := l.control.root.Remove(tx.name("before")); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		} else {
			tx.displaced = false
		}
	}
	if tx.journal != nil {
		_ = tx.journal.Close()
		tx.journal = nil
	}
	if err := l.control.root.Remove(tx.name("record")); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (tx *authoringLocalTransaction) recovery() authoringRecovery {
	r := authoringRecovery{Scope: tx.preview.Scope, Path: tx.preview.Path, Record: filepath.Join(tx.lock.control.path, tx.name("record")), Files: []string{}}
	if tx.displaced {
		r.Files = append(r.Files, filepath.Join(tx.lock.control.path, tx.name("before")))
	}
	if tx.undoDisplaced {
		r.Files = append(r.Files, filepath.Join(tx.lock.control.path, tx.name("undo")))
	}
	return r
}
func (tx *authoringLocalTransaction) close() {
	if tx.journal != nil {
		_ = tx.journal.Close()
	}
}
