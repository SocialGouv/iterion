// Package authoringlock owns the lock that serializes the writers of one
// stored bot file: its identity (where it lives and what it is called), its
// exclusive take, and the contract of the control directory that holds it.
//
// The server's editors take this lock around every save. The writers outside
// them — the catalogue regeneration, the CLI formatters and migrators, the
// package installers — reach it from here, because the lock was born in
// pkg/server, the composition root no pkg package may import
// (internal/layerguard/layering_test.go).
//
// The lock is per FILE: its name is the hash of the file's own name, so two
// writers of two different files never wait on each other, and two spellings
// of ONE name — a different case, a different Unicode composition, a symlink
// alias — fold to one lock.
//
// The cost this package carries for its callers is not the flock: it is the
// control directory. That directory is shared state the server AUDITS at
// every take, and a writer that creates it with the wrong modes makes the
// server refuse every save of that file — strictly worse than the race the
// lock closes. The creation contract therefore lives here, once, instead of
// at each writer: 0700 directories, an ignore rule of exactly `*\n` in 0600,
// every control entry an owned private regular file with an expected name.
//
// What stays in pkg/server is what speaks HTTP: the stored-file check behind
// the editors' 409s, the recovery journal, and the publication of a candidate
// over a live file — and one gate this package deliberately does not carry:
// the server also refuses a control directory whose recovery data Git
// TRACKS, which costs a `git ls-files` per take. A writer reaching the lock
// from here is held to the modes and the names, not to the operator's index;
// it is the permissive side of that difference, so a take from here never
// turns the server's saves into refusals.
package authoringlock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/text/unicode/norm"
)

const (
	// RecoveryDirName is the directory the control directory lives in, beside
	// the files it answers for. An operator-owned symlink there is a valid
	// location: a caller resolves it, never rewrites it.
	RecoveryDirName = ".iterion"
	// ControlDirName is the control directory itself, inside RecoveryDirName.
	ControlDirName = "authoring"
	// ExclusionName is the ignore rule that keeps the recovery data out of
	// the operator's commits.
	ExclusionName = ".gitignore"
	// ExclusionBody is the only body ExclusionName may hold. A control
	// directory whose rule says anything else is refused, never repaired:
	// the file belongs to whoever wrote it.
	ExclusionBody = "*\n"
	// dirMode and privateMode are the modes the contract creates with: a
	// directory only its owner may enter, a file only its owner may read.
	dirMode     = 0o700
	privateMode = 0o600
	// lockPollInterval is how often a blocked take retries. The wait itself
	// is bounded by the caller's context, never by this package.
	lockPollInterval = 20 * time.Millisecond
)

// controlEntryName matches every name the control directory may hold beside
// its ignore rule: one lock per file name (the hash of the folded name) and
// the per-save recovery objects, named by the save's identifier. The
// predicates below are what the two readers of this rule call — this take
// and the server's, which audits the same listing with its own error
// messages and its own test seams.
var controlEntryName = regexp.MustCompile(`^(?:[a-f0-9]{64}\.lock|[a-f0-9-]{36}\.(?:record|candidate|before|undo))$`)

// IsControlEntry answers whether the control directory may hold name.
func IsControlEntry(name string) bool {
	return name == ExclusionName || controlEntryName.MatchString(name)
}

// IsDisplacedEntry answers whether name is a live file moved aside by a
// publication. Those objects are retained even when an external writer
// replaced the live path with a symlink or another non-regular object, so
// they are never followed and never held to the private mode — the
// enclosing directory is what keeps a foreign writer out of them.
func IsDisplacedEntry(name string) bool {
	return strings.HasSuffix(name, ".before") || strings.HasSuffix(name, ".undo")
}

// IsPrivateEntry answers whether name must be an owned file nobody else may
// read: the ignore rule, a lock, a save's record. A candidate carries the
// mode of the file it is about to become (0644 for a creation), so holding
// it to the private mode would refuse every save made while a neighbour is
// mid-flight.
func IsPrivateEntry(name string) bool {
	return name == ExclusionName || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".record")
}

// ErrAliasParentChanged is a path whose symlink resolves into another
// directory than the one its caller opened: the name and its target do not
// share a parent, so one lock cannot answer for both. The server maps it to
// a conflict its client can retry.
var ErrAliasParentChanged = errors.New("authoring destination alias changed its parent")

// ErrDirectoryChanged is a directory that stopped being the one that was
// opened, between the moment it was seen and the moment it was pinned.
var ErrDirectoryChanged = errors.New("authoring directory changed while opening")

// Identity is the lock that governs one path: the control directory holding
// it, and its name inside it. Two paths with the same Identity are one lock.
type Identity struct {
	Control string
	Name    string
}

// Path is the lock file itself — what flock opens.
func (i Identity) Path() string { return filepath.Join(i.Control, i.Name) }

// LockFileName is the lock's name for a file's base name. Conservative
// folding also serializes differently cased spellings on case-insensitive
// filesystems — extra contention on sensitive ones is harmless. NFC
// coalesces Darwin's composed and decomposed aliases of one name.
func LockFileName(base string) string {
	folded := strings.ToLower(norm.NFC.String(NormalizeName(base)))
	sum := sha256.Sum256([]byte(folded))
	return hex.EncodeToString(sum[:]) + ".lock"
}

// CanonicalBase is the name the lock folds for path: the name its own
// symlink resolves to, when it is one, so an alias and its target take one
// lock. parent is the FileInfo of the physical directory the caller opened
// path in; a target that does not live in it answers ErrAliasParentChanged,
// because the lock of another directory is not this caller's lock. A path
// that is not there yet folds under its own name — the writer creating it
// holds the lock its editors hold.
func CanonicalBase(path string, parent os.FileInfo) (string, error) {
	base := filepath.Base(path)
	canonical, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return base, nil
	}
	if err != nil {
		return "", err
	}
	canonicalParent, err := os.Stat(filepath.Dir(canonical))
	if err != nil {
		return "", err
	}
	if !os.SameFile(parent, canonicalParent) {
		return "", ErrAliasParentChanged
	}
	return filepath.Base(canonical), nil
}

// Resolve answers the identity of the lock that governs path, as the
// filesystem stands: it creates nothing and takes nothing. A control
// directory that is not there yet is an error, not an empty answer — the
// caller that wants one created takes the lock.
func Resolve(path string) (Identity, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Identity{}, err
	}
	physical, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return Identity{}, err
	}
	parent, err := os.Stat(physical)
	if err != nil {
		return Identity{}, err
	}
	base, err := CanonicalBase(abs, parent)
	if err != nil {
		return Identity{}, err
	}
	recovery, err := filepath.EvalSymlinks(filepath.Join(physical, RecoveryDirName))
	if err != nil {
		return Identity{}, err
	}
	return Identity{Control: filepath.Join(recovery, ControlDirName), Name: LockFileName(base)}, nil
}

// OpenPinnedDir opens the directory at path and pins it: the descriptor it
// answers is bound to the directory the Lstat saw, so a rename, a removal or
// a substituted symlink under the caller cannot redirect what it opens next.
// A path that is not a directory, or is a symlink to one, is refused: the
// control directory is never reached through a link a writer can repoint.
func OpenPinnedDir(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("unsafe authoring directory %s", path)
	}
	file, err := OpenDirectory(path, info)
	if err != nil {
		return nil, nil, err
	}
	return file, info, nil
}

// EnsureDir creates a control directory named name under parent when it is
// not there, at 0700, and flushes the parent's entry — a concurrent creator
// may not have flushed it yet. A directory already there is left exactly as
// it is: its modes are audited by ValidateOwnership, never repaired, because
// a directory this process did not create belongs to whoever did.
func EnsureDir(parent *os.File, name string) error {
	if err := MkdirAt(parent, name); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return SyncDirectory(parent)
}

// EnsureExclusion writes the control directory's ignore rule when it is not
// there: exactly ExclusionBody, in 0600, created exclusively so two writers
// never both write it. An existing rule is never rewritten — CheckExclusion
// is what holds it to the contract.
func EnsureExclusion(control *os.File) error {
	f, err := OpenFileAt(control, ExclusionName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateMode)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(ExclusionBody)
	return errors.Join(writeErr, f.Sync(), f.Close(), SyncDirectory(control))
}

// CheckExclusion holds the control directory's ignore rule to the contract:
// it says exactly ExclusionBody, and it is a file nobody but its owner may
// read or write. An exclusive creator can be preempted between creating the
// rule and writing its two bytes, so an EMPTY file — and only an empty file
// — is waited for briefly; the wait is bounded by ctx and by its own
// deadline, and the rule is never rewritten.
func CheckExclusion(ctx context.Context, control *os.File) error {
	deadline := time.NewTimer(100 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		body, err := readExclusion(control)
		if err != nil {
			return err
		}
		if len(body) != 0 {
			if string(body) != ExclusionBody {
				return fmt.Errorf("unsafe authoring ignore file in %s: expected private %s exclusion", control.Name(), strings.TrimSpace(ExclusionBody))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("authoring ignore file remained empty")
		case <-tick.C:
		}
	}
}

// readExclusion reads the rule with room to spare: a file that is not the
// two bytes of ExclusionBody is refused for what it HOLDS, not for its
// length, so the read must be able to see more than the two bytes.
func readExclusion(control *os.File) ([]byte, error) {
	f, err := OpenFileAt(control, ExclusionName, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("unsafe authoring control file: %s", ExclusionName)
	}
	if err := ValidateOwnership(f, true); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, 64))
}

// AuditEntry holds one listed control entry to the contract: a name the
// directory may hold, a regular file, and — for the rule, a lock or a record
// — an owned file nobody else may read. An entry that is no longer there
// answers nil: a listing is a snapshot, and an entry a concurrent save
// published out of the directory holds nothing unsafe.
func AuditEntry(control *os.File, name string) error {
	if !IsControlEntry(name) {
		return fmt.Errorf("unexpected authoring control entry: %s", filepath.Join(control.Name(), name))
	}
	if IsDisplacedEntry(name) {
		return nil
	}
	f, err := OpenFileAt(control, name, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe authoring control file: %s", name)
	}
	if !IsPrivateEntry(name) {
		return nil
	}
	return ValidateOwnership(f, true)
}

// Lock is a held authoring lock: the exclusive flock on the control file
// that names one stored file. Release returns it. The lock file itself is
// never removed — deleting it loses the guarantee for every writer that
// holds the same name by another descriptor.
type Lock struct {
	identity Identity
	flock    *flock.Flock
	control  *os.File
}

// Identity is the lock this hold took.
func (l *Lock) Identity() Identity { return l.identity }

// Release gives the lock back and closes what the take opened.
func (l *Lock) Release() error {
	err := l.flock.Unlock()
	return errors.Join(err, l.flock.Close(), l.control.Close())
}

// Take creates the control directory for path when it is not there, holds it
// to the contract, and takes the file's lock exclusively. The wait for a lock
// another writer holds is bounded by ctx and by nothing else: a caller that
// must not block forever passes a deadline.
//
// Take is not re-entrant: a caller already holding this file's lock waits on
// itself until its context ends. The Lock it answers is released once —
// through Hold, or by one Release.
func Take(ctx context.Context, path string) (*Lock, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	physical, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	parent, parentInfo, err := OpenPinnedDir(physical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	base, err := CanonicalBase(abs, parentInfo)
	if err != nil {
		return nil, err
	}
	control, controlPath, err := openControlDir(ctx, parent, physical)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = control.Close()
		}
	}()
	identity := Identity{Control: controlPath, Name: LockFileName(base)}
	created, err := OpenFileAt(control, identity.Name, os.O_CREATE|os.O_RDWR, privateMode)
	if err != nil {
		return nil, err
	}
	lockInfo, err := created.Stat()
	if err == nil {
		if !lockInfo.Mode().IsRegular() {
			err = errors.New("authoring lock is not a regular file")
		} else {
			err = ValidateOwnership(created, true)
		}
	}
	if err := errors.Join(err, created.Close()); err != nil {
		return nil, err
	}
	held := flock.New(identity.Path(), flock.SetFlag(OpenFlags()), flock.SetPermissions(privateMode))
	won, err := held.TryLockContext(ctx, lockPollInterval)
	if err != nil {
		return nil, errors.Join(err, held.Close())
	}
	if !won {
		// The wait ends because the context did. A refusal with nothing to
		// name would answer (nil, nil) to a caller that is about to write.
		refusal := ctx.Err()
		if refusal == nil {
			refusal = errors.New("the authoring lock was not taken and the wait named no cause")
		}
		return nil, errors.Join(refusal, held.Close())
	}
	l := &Lock{identity: identity, flock: held, control: control}
	ok = true // the lock owns the control descriptor from here: Release closes it
	// The directory was audited before the lock was held, and an audit of
	// shared state is stale the moment it ends: the rule that the next bytes
	// of recovery data are written beside is answered for again, here, and
	// the lock flock holds is held to being the file this take opened.
	if err := errors.Join(CheckExclusion(ctx, control), sameLockFile(control, identity.Name, lockInfo, held)); err != nil {
		return nil, errors.Join(err, l.Release())
	}
	return l, nil
}

// Hold takes path's lock, runs fn under it and releases it. It is how a
// writer of one file reaches the lock without a leak: the release runs on
// every path out of fn, including a panic.
func Hold(ctx context.Context, path string, fn func() error) (err error) {
	l, takeErr := Take(ctx, path)
	if takeErr != nil {
		return takeErr
	}
	defer func() { err = errors.Join(err, l.Release()) }()
	return fn()
}

// openControlDir creates the recovery and control directories under an
// opened parent when they are not there, resolves the operator's symlink if
// RecoveryDirName is one, and holds both to the ownership contract. It
// answers the pinned control directory and its resolved path.
func openControlDir(ctx context.Context, parent *os.File, physical string) (*os.File, string, error) {
	if err := EnsureDir(parent, RecoveryDirName); err != nil {
		return nil, "", err
	}
	// An operator-owned .iterion symlink may point to another disk. It is a
	// valid lock location; what it must not be is a dangling entry.
	recoveryPath, err := filepath.EvalSymlinks(filepath.Join(physical, RecoveryDirName))
	if err != nil {
		return nil, "", fmt.Errorf("the authoring recovery directory %s resolves to nothing: %w", filepath.Join(physical, RecoveryDirName), err)
	}
	recovery, _, err := OpenPinnedDir(recoveryPath)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = recovery.Close() }()
	if err := ValidateOwnership(recovery, false); err != nil {
		return nil, "", err
	}
	if err := EnsureDir(recovery, ControlDirName); err != nil {
		return nil, "", err
	}
	controlPath := filepath.Join(recoveryPath, ControlDirName)
	control, _, err := OpenPinnedDir(controlPath)
	if err != nil {
		return nil, "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = control.Close()
		}
	}()
	if err := ValidateOwnership(control, true); err != nil {
		return nil, "", err
	}
	if err := EnsureExclusion(control); err != nil {
		return nil, "", err
	}
	if err := CheckExclusion(ctx, control); err != nil {
		return nil, "", err
	}
	names, err := listNames(control)
	if err != nil {
		return nil, "", err
	}
	for _, name := range names {
		if err := AuditEntry(control, name); err != nil {
			return nil, "", err
		}
	}
	ok = true
	return control, controlPath, nil
}

// listNames reads the entries of a freshly opened directory. A descriptor
// answers its entries ONCE — the caller opens the directory for this read
// and never reads it twice, because rewinding a directory handle is not an
// operation every platform serves.
func listNames(dir *os.File) ([]string, error) { return dir.Readdirnames(-1) }

// sameLockFile holds the file flock locked to being the file the take
// opened and audited: a lock replaced between the two is another writer's
// file, and a hold on it guards nothing.
func sameLockFile(control *os.File, name string, opened os.FileInfo, held *flock.Flock) error {
	f, err := OpenFileAt(control, name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	current, err := f.Stat()
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	locked, err := held.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) || !os.SameFile(current, locked) {
		return errors.New("authoring lock file changed")
	}
	return nil
}
