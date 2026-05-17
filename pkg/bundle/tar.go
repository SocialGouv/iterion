package bundle

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultMaxBundleBytes is the upper bound on the total uncompressed
// size of a bundle archive. Configurable via ITERION_BUNDLE_MAX_BYTES.
const defaultMaxBundleBytes = 256 * 1024 * 1024 // 256 MiB

// defaultMaxBundleEntries caps the number of tar entries to defuse
// archives that try to exhaust inode limits via many tiny files.
const defaultMaxBundleEntries = 10000

// extractTarGz extracts a gzip-decompressed tar stream into dest. The
// caller is responsible for gzip-decompressing the input — extractTarGz
// reads raw tar bytes — and for streaming the stream through a SHA-256
// hasher (so the bundle hash covers the uncompressed tar bytes,
// independent of gzip's non-deterministic output).
//
// Safety guards:
//   - rejects paths containing ".." or absolute components;
//   - rejects symlinks (and other non-Reg/Dir types);
//   - rejects entries whose resolved path escapes dest;
//   - enforces total bytes and entry count caps.
//
// Returns the number of regular files written.
func extractTarGz(r io.Reader, dest string) (int, error) {
	maxBytes := envInt64("ITERION_BUNDLE_MAX_BYTES", defaultMaxBundleBytes)
	maxEntries := envInt("ITERION_BUNDLE_MAX_ENTRIES", defaultMaxBundleEntries)

	absDest, err := filepath.Abs(dest)
	if err != nil {
		return 0, fmt.Errorf("bundle: resolve dest %s: %w", dest, err)
	}
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return 0, fmt.Errorf("bundle: mkdir %s: %w", absDest, err)
	}

	tr := tar.NewReader(r)
	var totalBytes int64
	entries := 0
	written := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return written, fmt.Errorf("bundle: read tar entry: %w", err)
		}
		entries++
		if entries > maxEntries {
			return written, fmt.Errorf("bundle: too many entries (>%d)", maxEntries)
		}
		if err := guardEntry(hdr); err != nil {
			return written, err
		}
		target, err := safeJoin(absDest, hdr.Name)
		if err != nil {
			return written, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return written, fmt.Errorf("bundle: mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size > maxBytes-totalBytes {
				return written, fmt.Errorf("bundle: total size exceeds limit (%d bytes)", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return written, fmt.Errorf("bundle: mkdir parent %s: %w", filepath.Dir(target), err)
			}
			// O_TRUNC because we may be extracting into a cache dir that
			// was partially populated by an earlier failed run; we want
			// the new bytes, not an append. O_NOFOLLOW closes the TOCTOU
			// gap between assertNoEscapingSymlink (which checks at safeJoin
			// time) and this open — a process with write access to the
			// destination dir could otherwise drop a symlink at `target`
			// in the interim and redirect the write out of the dest.
			// The exported extractTarGz contract is also documented as
			// "dest must be exclusively owned by the caller".
			f, err := openFileNoFollow(target, fileMode(hdr.Mode))
			if err != nil {
				return written, fmt.Errorf("bundle: create %s: %w", target, err)
			}
			n, copyErr := io.CopyN(f, tr, hdr.Size)
			closeErr := f.Close()
			if copyErr != nil && !errors.Is(copyErr, io.EOF) {
				return written, fmt.Errorf("bundle: write %s: %w", target, copyErr)
			}
			if closeErr != nil {
				return written, fmt.Errorf("bundle: close %s: %w", target, closeErr)
			}
			totalBytes += n
			written++
		default:
			return written, fmt.Errorf("bundle: unsupported entry type %q for %s (only regular files and directories allowed)", string(hdr.Typeflag), hdr.Name)
		}
	}
	return written, nil
}

// guardEntry checks the tar header for the simple bans (absolute paths,
// "..", non-portable separators) before any filesystem operation.
func guardEntry(hdr *tar.Header) error {
	clean := filepath.ToSlash(filepath.Clean(hdr.Name))
	if clean == "" || clean == "." {
		return nil
	}
	if strings.HasPrefix(clean, "/") {
		return fmt.Errorf("bundle: absolute path not allowed: %s", hdr.Name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return fmt.Errorf("bundle: path traversal not allowed: %s", hdr.Name)
		}
	}
	return nil
}

// safeJoin joins root and rel, then verifies the result stays under
// root. Defends against symlink-free traversal: a tar entry named
// `./foo/../../etc/passwd` would clean to `../etc/passwd` and escape
// even without symlinks.
//
// Also walks every existing component of the resolved path and
// rejects the entry if any intermediate component is a symlink that
// resolves outside root. Without that check a pre-existing
// `dest/foo → /etc` lets a tar entry `foo/bar.txt` land outside root
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

// hashingReader wraps a Reader and feeds every byte read into the given
// hash. Use it to compute a stable checksum over the uncompressed tar
// stream while the tar reader consumes it.
type hashingReader struct {
	r io.Reader
	h hash.Hash
}

func newHashingReader(r io.Reader) *hashingReader {
	return &hashingReader{r: r, h: sha256.New()}
}

func (hr *hashingReader) Read(p []byte) (int, error) {
	n, err := hr.r.Read(p)
	if n > 0 {
		hr.h.Write(p[:n])
	}
	return n, err
}

func (hr *hashingReader) Sum() string {
	return hex.EncodeToString(hr.h.Sum(nil))
}

// fileMode masks the supplied tar mode to the subset we permit on disk.
// Tar headers can carry sticky/suid bits; bundles must not.
func fileMode(m int64) os.FileMode {
	return os.FileMode(m) & 0o777
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
