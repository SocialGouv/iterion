package bundle

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// Archive magic bytes used to auto-detect the bundle container format.
// New bundles are ZIP (PK\x03\x04); legacy bundles are gzip (\x1f\x8b)
// wrapping a tar stream. Both are read transparently.
var (
	zipMagic  = []byte{0x50, 0x4b, 0x03, 0x04} // "PK\x03\x04"
	gzipMagic = []byte{0x1f, 0x8b}
)

func hasPrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && bytes.Equal(b[:len(prefix)], prefix)
}

// botFileNames is the set of accepted workflow source file names at the
// bundle root. The canonical name is `main.bot` (familiar `main.go` /
// `main.rs` convention, independent of the bundle directory name).
var botFileNames = []string{MainBotFile}

// Detect classifies path as a plain `.bot` file, a `.botz` archive, or a
// directory bundle.
func Detect(path string) (Kind, error) {
	info, err := os.Stat(path)
	if err != nil {
		return KindBot, fmt.Errorf("bundle: stat %s: %w", path, err)
	}
	if info.IsDir() {
		// Directory bundle: look for `main.bot` at the root.
		for _, name := range botFileNames {
			if _, err := os.Stat(filepath.Join(path, name)); err == nil {
				return KindBundleDir, nil
			}
		}
		return KindBot, fmt.Errorf("bundle: %s is a directory but contains no main.bot at root", path)
	}
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".botz") {
		return KindBundle, nil
	}
	if workflowfile.IsWorkflowFile(lower) {
		return KindBot, nil
	}
	return KindBot, fmt.Errorf("bundle: unsupported workflow extension for %s (expected .bot or .botz)", path)
}

// Open loads a `.botz` archive from path, extracting it to a stable
// content-addressed location under cacheRoot. Returns the Bundle, a
// cleanup function (no-op when cached; per-run extraction would clean
// up here), and an error.
//
// cacheRoot defaults to `<UserCacheDir>/iterion/bundles` when empty.
// Extraction is idempotent — concurrent runs of the same bundle share
// the cache via a `.ready` sentinel.
func Open(path, cacheRoot string) (*Bundle, func() error, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("bundle: resolve %s: %w", path, err)
	}
	if cacheRoot == "" {
		cacheRoot, err = defaultCacheRoot()
		if err != nil {
			return nil, nil, err
		}
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return nil, nil, fmt.Errorf("bundle: mkdir cache %s: %w", cacheRoot, err)
	}

	// Extract the archive into a temporary directory so concurrent calls
	// can't race; on success we move it into the cache slot under the
	// content hash, or skip the move when an equivalent slot already
	// exists. The container format (ZIP or legacy tar.gz) is auto-detected
	// from the leading magic bytes. The content hash is computed AFTER
	// extraction by walking the extracted tree, so it covers the logical
	// content and is identical across container formats.
	tmpDir, err := os.MkdirTemp(cacheRoot, "extract-")
	if err != nil {
		return nil, nil, fmt.Errorf("bundle: mkdir tmp: %w", err)
	}
	cleanupTmp := func() { _ = os.RemoveAll(tmpDir) }

	if err := extractArchiveFile(abs, tmpDir); err != nil {
		cleanupTmp()
		return nil, nil, err
	}
	hash, err := collectContentHash(tmpDir)
	if err != nil {
		cleanupTmp()
		return nil, nil, err
	}

	// Use the full hash on non-Windows hosts to make collision
	// astronomically rare even against an adversary who controls
	// bundle contents (the previous 64-bit truncation was crackable
	// under <2^32 work, opening a cache-poisoning path). Windows
	// keeps the 16-char truncation to stay under MAX_PATH for deeply
	// nested skill dirs. Sub-shard by the first two chars so the
	// cache root doesn't become one mega-directory.
	slotName := hash
	if runtime.GOOS == "windows" {
		slotName = hash[:16]
	}
	shard := slotName[:2]
	cacheSlot := filepath.Join(cacheRoot, shard, slotName)
	readySentinel := filepath.Join(cacheSlot, ".ready")
	if _, err := os.Stat(readySentinel); err == nil {
		// Slot already populated by an earlier (or concurrent) run.
		cleanupTmp()
	} else {
		// Race-safe install: write the .ready sentinel and lock file
		// INSIDE tmpDir before the rename. The rename atomically
		// publishes a slot that is already complete from a consumer's
		// point of view. The previous order (rename → writeLock →
		// touch sentinel) had two observable intermediate states a
		// concurrent reader could trip on.
		if err := writeLock(tmpDir, hash, abs); err != nil {
			cleanupTmp()
			return nil, nil, err
		}
		if err := touch(filepath.Join(tmpDir, ".ready")); err != nil {
			cleanupTmp()
			return nil, nil, err
		}
		if err := os.MkdirAll(filepath.Join(cacheRoot, shard), 0o755); err != nil {
			cleanupTmp()
			return nil, nil, fmt.Errorf("bundle: create cache shard: %w", err)
		}
		if err := os.Rename(tmpDir, cacheSlot); err != nil {
			// Either a peer beat us to it (cacheSlot exists with a
			// sentinel) or the rename failed for another reason. Wait
			// briefly for the peer's sentinel to land, then re-stat.
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if _, statErr := os.Stat(readySentinel); statErr == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if _, statErr := os.Stat(readySentinel); statErr != nil {
				cleanupTmp()
				return nil, nil, fmt.Errorf("bundle: install cache slot %s: %w", cacheSlot, err)
			}
			cleanupTmp()
		}
	}

	b, err := assembleBundle(cacheSlot)
	if err != nil {
		return nil, nil, err
	}
	b.Hash = hash
	b.SourcePath = abs
	b.Kind = KindBundle
	return b, func() error { return nil }, nil
}

// ExtractArchive extracts a `.botz` stream from r into dest, applying the
// same path-traversal / size / symlink guards as Open. The container
// format (ZIP or legacy tar.gz) is auto-detected from the leading magic
// bytes. dest must be a directory the caller exclusively owns (it is
// created if missing). Returns the number of regular files written.
//
// archive/zip needs a ReaderAt + size and cannot stream a pipe, so the
// stream is read fully into memory first. Bundles are small (capped at
// ITERION_BUNDLE_MAX_BYTES uncompressed; the compressed upload is smaller
// still), so this is acceptable for the .botz-upload path this serves.
//
// Unlike Open it does NOT cache, content-hash, or validate the bundle
// structure — callers that need a validated Bundle follow with
// OpenDir(dest). This is the in-memory entry point behind .botz uploads,
// where the bytes arrive over HTTP rather than from a file on disk.
func ExtractArchive(r io.Reader, dest string) (int, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("bundle: read archive: %w", err)
	}
	return extractArchiveBytes(data, dest)
}

// extractArchiveFile auto-detects the container format of the bundle at
// path and extracts it into dest. ZIP uses zip.OpenReader (a ReaderAt
// straight off the file); tar.gz streams through gzip → tar.
func extractArchiveFile(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("bundle: open %s: %w", path, err)
	}
	defer f.Close()

	var magic [4]byte
	n, _ := io.ReadFull(f, magic[:])
	head := magic[:n]

	switch {
	case hasPrefix(head, zipMagic):
		zr, err := zip.OpenReader(path)
		if err != nil {
			return fmt.Errorf("bundle: open zip %s: %w", path, err)
		}
		defer zr.Close()
		_, err = extractZip(&zr.Reader, dest)
		return err
	case hasPrefix(head, gzipMagic):
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("bundle: seek %s: %w", path, err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("bundle: gzip %s: %w", path, err)
		}
		defer gz.Close()
		_, err = extractTarGz(gz, dest)
		return err
	default:
		return fmt.Errorf("bundle: unrecognised archive format for %s (expected zip or gzip)", path)
	}
}

// extractArchiveBytes auto-detects the container format of the in-memory
// bundle bytes and extracts them into dest.
func extractArchiveBytes(data []byte, dest string) (int, error) {
	switch {
	case hasPrefix(data, zipMagic):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return 0, fmt.Errorf("bundle: open zip: %w", err)
		}
		return extractZip(zr, dest)
	case hasPrefix(data, gzipMagic):
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return 0, fmt.Errorf("bundle: gzip: %w", err)
		}
		defer gz.Close()
		return extractTarGz(gz, dest)
	default:
		return 0, fmt.Errorf("bundle: unrecognised archive format (expected zip or gzip)")
	}
}

// OpenDir resolves an already-extracted bundle directory. Used by dev
// workflows and tests where authoring happens in-place.
func OpenDir(path string) (*Bundle, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("bundle: resolve %s: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("bundle: stat %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle: %s is not a directory", abs)
	}
	b, err := assembleBundle(abs)
	if err != nil {
		return nil, err
	}
	b.SourcePath = abs
	b.Kind = KindBundleDir
	return b, nil
}

// assembleBundle scans dir for the workflow source, manifest, and
// optional resource directories. Returns an error when no workflow
// source is present at the bundle root.
func assembleBundle(dir string) (*Bundle, error) {
	b := &Bundle{Dir: dir}
	for _, name := range botFileNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			b.IterPath = p
			break
		}
	}
	if b.IterPath == "" {
		return nil, fmt.Errorf("bundle: %s contains no main.bot at root", dir)
	}
	// Each layout dir resolves to its absolute path when present, "" when
	// absent. Driven by a table so the names live only in bundle.go.
	for _, d := range []struct {
		name string
		dst  *string
	}{
		{DirSkills, &b.SkillsDir},
		{DirPrompts, &b.PromptsDir},
		{DirAttachments, &b.AttachmentsDir},
		{DirPresets, &b.PresetsDir},
	} {
		p := filepath.Join(dir, d.name)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			*d.dst = p
		}
	}
	manifest, err := LoadManifest(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	if manifest == nil {
		manifest, err = LoadManifest(filepath.Join(dir, ManifestFileAlt))
		if err != nil {
			return nil, err
		}
	}
	b.Manifest = manifest
	return b, nil
}

// defaultCacheRoot returns the platform-specific cache directory for
// iterion bundles. Honours XDG_CACHE_HOME on Linux via os.UserCacheDir.
func defaultCacheRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("bundle: locate user cache dir: %w", err)
	}
	return filepath.Join(base, "iterion", "bundles"), nil
}

// writeLock persists the full hash and original archive path inside the
// cache slot. Lets `iterion resume` re-locate the source archive when
// the cache has been GC'd between runs.
func writeLock(dir, fullHash, source string) error {
	body := fmt.Sprintf("hash: %s\nsource: %s\n", fullHash, source)
	return os.WriteFile(filepath.Join(dir, "bundle.lock"), []byte(body), 0o600)
}

func touch(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("bundle: touch %s: %w", path, err)
	}
	return f.Close()
}
