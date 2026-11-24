//go:build unix

package kubernetes

import (
	"errors"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"

	"golang.org/x/sys/unix"
)

// drainDir is one directory of a drain's host tree, held open by its
// descriptor. Every entry the drain judges, writes or removes is named
// relative to it by ONE component, so no path the kernel resolves on the
// drain's behalf crosses a link: a directory is entered with O_NOFOLLOW
// (child), and every other call acts on the entry itself — fstatat,
// unlinkat, renameat and symlinkat never follow the name they are handed.
// A link standing anywhere on a member's path can therefore stop the
// member, never send its write somewhere else.
//
// And a name reaches an entry only where the directory lists it under
// exactly that spelling (spells): a filesystem that folds names — case on
// APFS's default or a casefold directory, Unicode normalization on APFS —
// answers a lookup of one spelling with the entry another names, which
// would give one entry two keys. Such a name reaches nothing
// (entryFolded, errFoldedName): it can stop a member, never land it on an
// entry its name does not spell.
type drainDir struct {
	fd int
	// path is where the directory stands — the drain's root joined with
	// its key — for messages alone: no call resolves it.
	path string
	// key is the directory's key under the drain's root, "" for the root.
	key string
	// spelling is the tree's, shared by every directory of it.
	spelling *drainSpelling
}

// drainSpelling is what one drain's tree knows of the spelling its entries
// stand under (drainDir.spells).
type drainSpelling struct {
	// keys is every key the drain knows an entry stands at under exactly
	// that spelling: each entry the drain created or renamed there — the
	// name it handed the kernel where nothing stood under any spelling,
	// kept as handed: a filesystem that folds case preserves it, APFS
	// preserves normalization (HFS+, which rewrites a non-ASCII name to
	// its decomposed form, does not) — and each name a listing returned.
	// An entry the drain removes leaves it.
	keys map[string]bool
	// listings counts the directory listings the drain ran: what a key it
	// does not know yet costs.
	listings int
}

// drainPathMax bounds the absolute path of an entry the drain lands
// (drainTree.fits): the drain names every entry by descriptor, but the
// host's own readers — the run file store, the upload's walk — name it by
// its path, so a path they could not name is one the drain declines.
const drainPathMax = unix.PathMax

// drainFold, when set, is the filesystem's own folding of a name under d:
// it returns the spelling of the entry the kernel would resolve name to.
// Unset — on every host — the kernel resolves each name itself. No
// filesystem this package's tests run on folds names, so a test sets it to
// stand for one (foldDrainNames).
var drainFold func(d *drainDir, name string) string

// sys is the name the drain hands the kernel for name under d.
func (d *drainDir) sys(name string) string {
	if drainFold == nil {
		return name
	}
	return drainFold(d, name)
}

// openDrainRoot opens the drain's root, path as drainRoot resolved it —
// never a link standing there since.
func openDrainRoot(path string) (*drainDir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &drainDir{fd: fd, path: path, spelling: &drainSpelling{keys: map[string]bool{}}}, nil
}

// close releases the descriptor. A directory opened read-only holds
// nothing to flush, so its close has no failure to report.
func (d *drainDir) close() {
	_ = unix.Close(d.fd)
}

// spells reports whether the entry standing at name under d is listed
// under exactly that spelling. A key the drain does not know yet costs
// one listing of d, which records every name d holds.
func (d *drainDir) spells(name string) (bool, error) {
	if d.spelling.keys[joinKey(d.key, name)] {
		return true, nil
	}
	if _, err := d.names(); err != nil {
		return false, err
	}
	return d.spelling.keys[joinKey(d.key, name)], nil
}

// child opens the directory name under d. A name standing as anything but
// a directory — a link above all, whatever it points at — is
// errNotDrainDir; one the directory lists under another spelling is
// errFoldedName; a missing one is fs.ErrNotExist.
func (d *drainDir) child(name string) (*drainDir, error) {
	fd, err := unix.Openat(d.fd, d.sys(name), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		spelled, err := d.spells(name)
		if err != nil || !spelled {
			_ = unix.Close(fd)
			if err == nil {
				err = errFoldedName
			}
			return nil, err
		}
		return &drainDir{fd: fd, path: d.join(name), key: joinKey(d.key, name), spelling: d.spelling}, nil
	}
	// The errno O_NOFOLLOW answers a link with differs between kernels
	// (ELOOP, ENOTDIR, EMLINK): the entry itself says what it is.
	switch kind, kindErr := d.kind(name); {
	case kindErr != nil, kind == entryNone, kind == entryDir:
	case kind == entryFolded:
		return nil, errFoldedName
	default:
		return nil, errNotDrainDir
	}
	return nil, &os.PathError{Op: "open", Path: d.join(name), Err: err}
}

// kind is what stands at name under d, the entry itself — entryNone when
// nothing does, entryFolded when the entry the name reaches is listed
// under another spelling.
func (d *drainDir) kind(name string) (entryKind, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, d.sys(name), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return entryNone, nil
		}
		return entryNone, &os.PathError{Op: "lstat", Path: d.join(name), Err: err}
	}
	spelled, err := d.spells(name)
	if err != nil {
		return entryNone, err
	}
	if !spelled {
		return entryFolded, nil
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return entryDir, nil
	case unix.S_IFREG:
		return entryFile, nil
	case unix.S_IFLNK:
		return entryLink, nil
	}
	return entryOther, nil
}

// readlink is the target of the link at name under d.
func (d *drainDir) readlink(name string) (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(d.fd, d.sys(name), buf)
		if err != nil {
			return "", &os.PathError{Op: "readlink", Path: d.join(name), Err: err}
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// mkdir creates the directory name under d.
func (d *drainDir) mkdir(name string) error {
	if err := unix.Mkdirat(d.fd, d.sys(name), 0o755); err != nil {
		return &os.PathError{Op: "mkdir", Path: d.join(name), Err: err}
	}
	d.spelling.keys[joinKey(d.key, name)] = true
	return nil
}

// remove unlinks the entry at name under d — itself, a link included,
// never what it points at; dir removes an empty directory.
func (d *drainDir) remove(name string, dir bool) error {
	flags := 0
	if dir {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(d.fd, d.sys(name), flags); err != nil {
		return &os.PathError{Op: "remove", Path: d.join(name), Err: err}
	}
	delete(d.spelling.keys, joinKey(d.key, name))
	return nil
}

// rename moves the entry at from over the one at to, both under d: what
// stands at to — a file, a link — is replaced, never written through.
func (d *drainDir) rename(from, to string) error {
	if err := unix.Renameat(d.fd, d.sys(from), d.fd, d.sys(to)); err != nil {
		return &os.PathError{Op: "rename", Path: d.join(to), Err: err}
	}
	delete(d.spelling.keys, joinKey(d.key, from))
	d.spelling.keys[joinKey(d.key, to)] = true
	return nil
}

// drainStageTries bounds the staged names one write draws: a 64-bit draw
// that collides is an entry nobody else names this way, and a run of them
// is the host's failure, never a reason to loop.
const drainStageTries = 16

// stagedName is a fresh staged name: a prefix and a 64-bit draw, short
// whatever the member's own name — a staged sibling fits wherever the
// member does.
func stagedName(prefix string) string {
	return prefix + strconv.FormatUint(rand.Uint64(), 36)
}

// createStaged creates a new file under d for a member's bytes, under a
// staged name, and returns it with that name.
func (d *drainDir) createStaged() (*os.File, string, error) {
	for range drainStageTries {
		name := stagedName(".drain-")
		fd, err := unix.Openat(d.fd, d.sys(name), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", &os.PathError{Op: "create", Path: d.join(name), Err: err}
		}
		return os.NewFile(uintptr(fd), d.join(name)), name, nil
	}
	return nil, "", &os.PathError{Op: "create", Path: d.join(".drain-*"), Err: unix.EEXIST}
}

// symlinkStaged creates a link to target under d, under a staged name, and
// returns that name.
func (d *drainDir) symlinkStaged(target string) (string, error) {
	for range drainStageTries {
		name := stagedName(".drain-link-")
		err := unix.Symlinkat(target, d.fd, d.sys(name))
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", &os.PathError{Op: "symlink", Path: d.join(name), Err: err}
		}
		return name, nil
	}
	return "", &os.PathError{Op: "symlink", Path: d.join(".drain-link-*"), Err: unix.EEXIST}
}

// openFile opens the regular file at name under d for reading: never a
// link at name (O_NOFOLLOW), never blocking on a fifo (O_NONBLOCK), and
// errNotDrainFile for anything but a regular file listed under exactly
// that name — the file opened is the one judged. A missing one is
// fs.ErrNotExist.
func (d *drainDir) openFile(name string) (*os.File, error) {
	fd, err := unix.Openat(d.fd, d.sys(name), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if kind, kindErr := d.kind(name); kindErr == nil && kind != entryNone && kind != entryFile {
			return nil, errNotDrainFile
		}
		return nil, &os.PathError{Op: "open", Path: d.join(name), Err: err}
	}
	f := os.NewFile(uintptr(fd), d.join(name))
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errNotDrainFile
	}
	spelled, err := d.spells(name)
	if err != nil || !spelled {
		_ = f.Close()
		if err == nil {
			err = errNotDrainFile
		}
		return nil, err
	}
	return f, nil
}

// names lists the entries under d, sorted — a walk of the tree meets them
// in one order whatever order the directory keeps — and records each as
// spelled.
func (d *drainDir) names() ([]string, error) {
	fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: d.path, Err: err}
	}
	f := os.NewFile(uintptr(fd), d.path)
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	d.spelling.listings++
	sort.Strings(names)
	for _, name := range names {
		d.spelling.keys[joinKey(d.key, name)] = true
	}
	return names, nil
}
