package bundle

import (
	"archive/tar"
	"archive/zip"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// defaultMaxBundleBytes is the upper bound on the total uncompressed
// size of a bundle archive. Configurable via ITERION_BUNDLE_MAX_BYTES.
const defaultMaxBundleBytes = 256 * 1024 * 1024 // 256 MiB

// defaultMaxBundleEntries caps the number of archive entries to defuse
// archives that try to exhaust inode limits via many tiny files.
const defaultMaxBundleEntries = 10000

// extractLimits captures the resolved caps and destination for one
// extraction, plus the running counters shared across entries.
type extractLimits struct {
	maxBytes   int64
	maxEntries int
	absDest    string

	totalBytes int64
	entries    int
	written    int
}

func newExtractLimits(dest string) (*extractLimits, error) {
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return nil, fmt.Errorf("bundle: resolve dest %s: %w", dest, err)
	}
	if err := os.MkdirAll(absDest, 0o700); err != nil {
		return nil, fmt.Errorf("bundle: mkdir %s: %w", absDest, err)
	}
	return &extractLimits{
		maxBytes:   envInt64("ITERION_BUNDLE_MAX_BYTES", defaultMaxBundleBytes),
		maxEntries: envInt("ITERION_BUNDLE_MAX_ENTRIES", defaultMaxBundleEntries),
		absDest:    absDest,
	}, nil
}

// extractTarGz extracts a gzip-decompressed tar stream into dest. The
// caller is responsible for gzip-decompressing the input — extractTarGz
// reads raw tar bytes. This is the legacy (tar.gz) bundle format, kept
// for backward compatibility; new bundles are ZIP (see extractZip).
//
// Safety guards:
//   - rejects paths containing ".." or absolute components;
//   - rejects symlinks (and other non-Reg/Dir types);
//   - rejects entries whose resolved path escapes dest;
//   - enforces total bytes and entry count caps.
//
// Returns the number of regular files written.
func extractTarGz(r io.Reader, dest string) (int, error) {
	lim, err := newExtractLimits(dest)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return lim.written, fmt.Errorf("bundle: read tar entry: %w", err)
		}
		if err := lim.countEntry(); err != nil {
			return lim.written, err
		}
		if err := guardName(hdr.Name); err != nil {
			return lim.written, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := lim.makeDir(hdr.Name); err != nil {
				return lim.written, err
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA marks regular files in legacy tar archives we must still read
			if err := lim.writeFile(hdr.Name, fileMode(hdr.Mode), hdr.Size, tr); err != nil {
				return lim.written, err
			}
		default:
			return lim.written, fmt.Errorf("bundle: unsupported entry type %q for %s (only regular files and directories allowed)", string(hdr.Typeflag), hdr.Name)
		}
	}
	return lim.written, nil
}

// extractZip extracts a ZIP archive (read via archive/zip, which needs a
// ReaderAt + size) into dest. This is the current `.botz` format. It
// applies the same path-traversal / size / symlink / cap guards as
// extractTarGz. Returns the number of regular files written.
func extractZip(zr *zip.Reader, dest string) (int, error) {
	lim, err := newExtractLimits(dest)
	if err != nil {
		return 0, err
	}
	for _, zf := range zr.File {
		if err := lim.countEntry(); err != nil {
			return lim.written, err
		}
		name := zf.Name
		if err := guardName(name); err != nil {
			return lim.written, err
		}
		mode := zf.Mode()
		// Directory entries carry a trailing slash by ZIP convention.
		if strings.HasSuffix(name, "/") || mode.IsDir() {
			if err := lim.makeDir(name); err != nil {
				return lim.written, err
			}
			continue
		}
		if mode&os.ModeSymlink != 0 {
			return lim.written, fmt.Errorf("bundle: symlinks not allowed (%s)", name)
		}
		if !mode.IsRegular() {
			return lim.written, fmt.Errorf("bundle: unsupported entry type for %s (only regular files and directories allowed)", name)
		}
		rc, err := zf.Open()
		if err != nil {
			return lim.written, fmt.Errorf("bundle: open zip entry %s: %w", name, err)
		}
		// zf.UncompressedSize64 is advisory (a crafted archive may lie),
		// so writeFile bounds the copy by the remaining byte budget and
		// stops as soon as the cap is exceeded.
		writeErr := lim.writeFile(name, fileMode(int64(mode.Perm())), int64(zf.UncompressedSize64), rc)
		_ = rc.Close()
		if writeErr != nil {
			return lim.written, writeErr
		}
	}
	return lim.written, nil
}

func (lim *extractLimits) countEntry() error {
	lim.entries++
	if lim.entries > lim.maxEntries {
		return fmt.Errorf("bundle: too many entries (>%d)", lim.maxEntries)
	}
	return nil
}

func (lim *extractLimits) makeDir(name string) error {
	target, err := safeJoin(lim.absDest, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("bundle: mkdir %s: %w", target, err)
	}
	return nil
}

// writeFile writes one regular-file entry. declaredSize is the archive's
// claimed uncompressed size (used for the up-front budget check); the
// actual bytes copied are bounded by the remaining budget so a lying
// header cannot exhaust disk.
func (lim *extractLimits) writeFile(name string, mode os.FileMode, declaredSize int64, src io.Reader) error {
	target, err := safeJoin(lim.absDest, name)
	if err != nil {
		return err
	}
	remaining := lim.maxBytes - lim.totalBytes
	if declaredSize > remaining {
		return fmt.Errorf("bundle: total size exceeds limit (%d bytes)", lim.maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("bundle: mkdir parent %s: %w", filepath.Dir(target), err)
	}
	// O_TRUNC because we may be extracting into a cache dir that was
	// partially populated by an earlier failed run; we want the new
	// bytes, not an append. O_NOFOLLOW closes the TOCTOU gap between
	// assertNoEscapingSymlink (which checks at safeJoin time) and this
	// open — a process with write access to the destination dir could
	// otherwise drop a symlink at `target` in the interim and redirect
	// the write out of the dest.
	f, err := openFileNoFollow(target, mode)
	if err != nil {
		return fmt.Errorf("bundle: create %s: %w", target, err)
	}
	// Copy at most remaining+1 bytes so an over-long body (header lied
	// about its size) is detected rather than silently truncated.
	n, copyErr := io.CopyN(f, src, remaining+1)
	closeErr := f.Close()
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return fmt.Errorf("bundle: write %s: %w", target, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("bundle: close %s: %w", target, closeErr)
	}
	if n > remaining {
		return fmt.Errorf("bundle: total size exceeds limit (%d bytes)", lim.maxBytes)
	}
	lim.totalBytes += n
	lim.written++
	return nil
}

// guardName checks an archive entry name for the simple bans (absolute
// paths, "..", non-portable separators) before any filesystem operation.
func guardName(name string) error {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "" || clean == "." {
		return nil
	}
	if strings.HasPrefix(clean, "/") {
		return fmt.Errorf("bundle: absolute path not allowed: %s", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return fmt.Errorf("bundle: path traversal not allowed: %s", name)
		}
	}
	return nil
}

// collectContentHash walks an extracted bundle directory and computes the
// stable, format-independent content hash (see contentHasher): the sorted
// sequence of (relative-path, file-bytes). Because the writer (PackDir)
// hashes the same logical content from the source tree, a freshly-packed
// ZIP bundle and a legacy tar.gz bundle holding identical files produce
// the same hash.
func collectContentHash(dir string) (string, error) {
	var files []string
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("bundle: hash walk %s: %w", dir, walkErr)
	}
	sort.Strings(files)

	hasher := newContentHasher()
	for _, rel := range files {
		hasher.AddFile(rel)
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("bundle: hash open %s: %w", rel, err)
		}
		_, copyErr := io.Copy(hasher, f)
		_ = f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("bundle: hash read %s: %w", rel, copyErr)
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// safeJoin joins root and rel, then verifies the result stays under
// root. Defends against symlink-free traversal: an entry named
// `./foo/../../etc/passwd` would clean to `../etc/passwd` and escape
// even without symlinks.
//
// Also walks every existing component of the resolved path and
// rejects the entry if any intermediate component is a symlink that
// resolves outside root. Without that check a pre-existing
// `dest/foo → /etc` lets an entry `foo/bar.txt` land outside root
// even though the lexical join stays inside (the OS follows the
// symlink at open time).
func safeJoin(root, rel string) (string, error) {
	joined := filepath.Join(root, filepath.FromSlash(rel))
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("bundle: resolve %s: %w", rel, err)
	}
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("bundle: entry escapes bundle root: %s", rel)
	}
	if err := assertNoEscapingSymlink(root, abs); err != nil {
		return "", err
	}
	return abs, nil
}

// assertNoEscapingSymlink walks every existing prefix of abs (root..abs)
// and refuses the path if a component is a symlink whose resolved
// target escapes root. New (not-yet-created) suffix components are
// ignored — they cannot be symlinks since they don't exist.
func assertNoEscapingSymlink(root, abs string) error {
	if !strings.HasPrefix(abs, root) {
		return fmt.Errorf("bundle: internal: abs %s outside root %s", abs, root)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return fmt.Errorf("bundle: rel %s: %w", abs, err)
	}
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // remaining suffix doesn't exist yet
		}
		if err != nil {
			return fmt.Errorf("bundle: stat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(cur)
		if err != nil {
			return fmt.Errorf("bundle: eval symlink %s: %w", cur, err)
		}
		if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			return fmt.Errorf("bundle: refusing entry: component %s is a symlink escaping bundle root", cur)
		}
	}
	return nil
}

// fileMode masks the supplied mode to the subset we permit on disk.
// Archive headers can carry sticky/suid bits; bundles must not. We also
// strip group/other bits (& 0o700): bundle files (skills, recipes, the
// .bot) are read by the iterion process only, so a world-readable
// extraction would expose potentially-sensitive prompts to other UIDs on
// a shared host.
func fileMode(m int64) os.FileMode {
	return os.FileMode(m) & 0o700
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envInt64(name string, def int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
