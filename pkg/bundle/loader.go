package bundle

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// botFileNames is the set of accepted workflow source file names at the
// bundle root. Both `.iter` and `.bot` are recognised — they have
// identical semantics (see README).
var botFileNames = []string{"bot.iter", "bot.bot"}

// Detect classifies path as a plain `.iter`/`.bot` file, a `.botz`
// archive, or a directory bundle. The classifier reads the extension
// first and falls back to gzip magic bytes for files without a
// recognised extension.
func Detect(path string) (Kind, error) {
	info, err := os.Stat(path)
	if err != nil {
		return KindIter, fmt.Errorf("bundle: stat %s: %w", path, err)
	}
	if info.IsDir() {
		// Directory bundle: look for a `bot.iter`/`bot.bot` at the root.
		for _, name := range botFileNames {
			if _, err := os.Stat(filepath.Join(path, name)); err == nil {
				return KindBundleDir, nil
			}
		}
		return KindIter, fmt.Errorf("bundle: %s is a directory but contains no bot.iter at root", path)
	}
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".botz") {
		return KindBundle, nil
	}
	if strings.HasSuffix(lower, ".iter") || strings.HasSuffix(lower, ".bot") {
		return KindIter, nil
	}
	// Magic-byte fallback for files with unrecognised extensions.
	if isGzipFile(path) {
		return KindBundle, nil
	}
	return KindIter, nil
}

func isGzipFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 2)
	n, _ := io.ReadFull(f, buf)
	return n == 2 && buf[0] == 0x1f && buf[1] == 0x8b
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
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return nil, nil, fmt.Errorf("bundle: mkdir cache %s: %w", cacheRoot, err)
	}

	// Stream the archive once: gunzip → hash + extract. We extract
	// into a temporary directory keyed by the source path mtime so
	// concurrent calls can't race; on success we move it into the
	// cache slot under the content hash, or skip the move when an
	// equivalent slot already exists.
	tmpDir, err := os.MkdirTemp(cacheRoot, "extract-")
	if err != nil {
		return nil, nil, fmt.Errorf("bundle: mkdir tmp: %w", err)
	}
	cleanupTmp := func() { _ = os.RemoveAll(tmpDir) }

	f, err := os.Open(abs)
	if err != nil {
		cleanupTmp()
		return nil, nil, fmt.Errorf("bundle: open %s: %w", abs, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		cleanupTmp()
		return nil, nil, fmt.Errorf("bundle: gzip %s: %w", abs, err)
	}
	defer gz.Close()

	hr := newHashingReader(gz)
	if _, err := extractTarGz(hr, tmpDir); err != nil {
		cleanupTmp()
		return nil, nil, err
	}
	hash := hr.Sum()

	// Truncate the hash for the directory name to avoid Windows long-path
	// limits; the full hash is recoverable from the lock file we write.
	cacheSlot := filepath.Join(cacheRoot, hash[:16])
	readySentinel := filepath.Join(cacheSlot, ".ready")
	if _, err := os.Stat(readySentinel); err == nil {
		// Slot already populated by an earlier (or concurrent) run.
		cleanupTmp()
	} else {
		// Atomic install: rename tmpDir → cacheSlot. If a concurrent
		// process beat us to it, accept the slot they created.
		if err := os.Rename(tmpDir, cacheSlot); err != nil {
			// Either the cacheSlot now exists (race won by peer) or a
			// rename across filesystems would fail; in either case we
			// fall back to checking the sentinel.
			if _, statErr := os.Stat(readySentinel); statErr != nil {
				cleanupTmp()
				return nil, nil, fmt.Errorf("bundle: install cache slot %s: %w", cacheSlot, err)
			}
			cleanupTmp()
		} else {
			if err := writeLock(cacheSlot, hash, abs); err != nil {
				return nil, nil, err
			}
			if err := touch(readySentinel); err != nil {
				return nil, nil, err
			}
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
		return nil, fmt.Errorf("bundle: %s contains no bot.iter or bot.bot at root", dir)
	}
	if info, err := os.Stat(filepath.Join(dir, "skills")); err == nil && info.IsDir() {
		b.SkillsDir = filepath.Join(dir, "skills")
	}
	if info, err := os.Stat(filepath.Join(dir, "prompts")); err == nil && info.IsDir() {
		b.PromptsDir = filepath.Join(dir, "prompts")
	}
	if info, err := os.Stat(filepath.Join(dir, "attachments")); err == nil && info.IsDir() {
		b.AttachmentsDir = filepath.Join(dir, "attachments")
	}
	manifest, err := LoadManifest(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return nil, err
	}
	if manifest == nil {
		manifest, err = LoadManifest(filepath.Join(dir, "manifest.yml"))
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
	return os.WriteFile(filepath.Join(dir, "bundle.lock"), []byte(body), 0o644)
}

func touch(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("bundle: touch %s: %w", path, err)
	}
	return f.Close()
}
