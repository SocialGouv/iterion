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
type drainDir struct {
	fd int
	// path is where the directory stands — the drain's root joined with
	// its key — for messages alone: no call resolves it.
	path string
}

// drainPathMax bounds the absolute path of an entry the drain lands
// (drainTree.fits): the drain names every entry by descriptor, but the
// host's own readers — the run file store, the upload's walk — name it by
// its path, so a path they could not name is one the drain declines.
const drainPathMax = unix.PathMax

// openDrainRoot opens the drain's root, path as drainRoot resolved it.
func openDrainRoot(path string) (*drainDir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return &drainDir{fd: fd, path: path}, nil
}

// close releases the descriptor. A directory opened read-only holds
// nothing to flush, so its close has no failure to report.
func (d *drainDir) close() {
	_ = unix.Close(d.fd)
}

// child opens the directory name under d. A name standing as anything but
// a directory — a link above all, whatever it points at — is
// errNotDrainDir; a missing one is fs.ErrNotExist.
func (d *drainDir) child(name string) (*drainDir, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err == nil {
		return &drainDir{fd: fd, path: d.join(name)}, nil
	}
	// The errno O_NOFOLLOW answers a link with differs between kernels
	// (ELOOP, ENOTDIR, EMLINK): the entry itself says what it is.
	if kind, kindErr := d.kind(name); kindErr == nil && kind != entryNone && kind != entryDir {
		return nil, errNotDrainDir
	}
	return nil, &os.PathError{Op: "open", Path: d.join(name), Err: err}
}

// kind is what stands at name under d, the entry itself — entryNone when
// nothing does.
func (d *drainDir) kind(name string) (entryKind, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return entryNone, nil
		}
		return entryNone, &os.PathError{Op: "lstat", Path: d.join(name), Err: err}
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
		n, err := unix.Readlinkat(d.fd, name, buf)
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
	if err := unix.Mkdirat(d.fd, name, 0o755); err != nil {
		return &os.PathError{Op: "mkdir", Path: d.join(name), Err: err}
	}
	return nil
}

// remove unlinks the entry at name under d — itself, a link included,
// never what it points at; dir removes an empty directory.
func (d *drainDir) remove(name string, dir bool) error {
	flags := 0
	if dir {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(d.fd, name, flags); err != nil {
		return &os.PathError{Op: "remove", Path: d.join(name), Err: err}
	}
	return nil
}

// rename moves the entry at from over the one at to, both under d: what
// stands at to — a file, a link — is replaced, never written through.
func (d *drainDir) rename(from, to string) error {
	if err := unix.Renameat(d.fd, from, d.fd, to); err != nil {
		return &os.PathError{Op: "rename", Path: d.join(to), Err: err}
	}
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
		fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
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
		err := unix.Symlinkat(target, d.fd, name)
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
// link at name (O_NOFOLLOW), and errNotDrainFile for anything but a regular
// file — the file opened is the one judged. A missing one is
// fs.ErrNotExist.
func (d *drainDir) openFile(name string) (*os.File, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
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
	return f, nil
}

// names lists the entries under d, sorted: a walk of the tree meets them
// in one order whatever order the directory keeps.
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
	sort.Strings(names)
	return names, nil
}
