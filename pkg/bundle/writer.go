package bundle

import (
	"archive/zip"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// zipEpoch is the fixed modification time stamped on every ZIP entry so
// the archive bytes are reproducible across machines and runs. The ZIP
// MS-DOS time field cannot represent dates before 1980-01-01, so that is
// the floor we pin to.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// skipPatterns matches paths the packer never includes in a .botz.
// Kept conservative: ignore iterion store, prior builds, OS metadata,
// editor scratch files. Project-level ignores (node_modules, dist, …)
// are the author's responsibility via their own scaffolding.
var skipPatterns = []string{
	".git",
	".iterion",
	".DS_Store",
}

// skipSuffixes matches filename suffixes the packer never includes.
var skipSuffixes = []string{
	".botz",
	".swp",
	"~",
}

// IsPackSkipped reports whether a bundle-relative path would be excluded
// from a .botz by [PackDir]. Exported so a scaffolder can prove the
// `.gitignore` it writes only lists things the packer already drops,
// instead of asserting that in a comment.
func IsPackSkipped(rel string) bool { return shouldSkip(rel) }

// PackResult summarises a successful PackDir invocation.
type PackResult struct {
	OutputPath string // absolute path of the .botz file
	Hash       string // SHA-256 of the logical bundle content — matches Bundle.Hash on Open
	Entries    int    // number of archive entries written (files + directories)
	BytesIn    int64  // sum of uncompressed file bytes
	BytesOut   int64  // size of the .botz on disk
}

// PackDir creates a .botz ZIP archive at outPath from the contents of
// srcDir. The bundle layout is the same as accepted by [Open] /
// [OpenDir]: main.bot at the root, plus optional manifest.yaml,
// skills/, prompts/, presets/, attachments/.
//
// The output is a standard ZIP archive (PK\x03\x04) so a downloaded
// `.botz` extracts with `unzip` / double-click. Older bundles were
// gzipped tarballs; [Open] / [ExtractArchive] still read those for
// backward compatibility (format auto-detect via magic bytes).
//
// The archive is deterministic — entries are sorted alphabetically,
// timestamps pinned, ownership stripped, modes uniformly set — so two
// PackDir invocations on the same directory tree produce byte-identical
// output.
//
// The content hash is computed over the LOGICAL bundle content (the
// sorted sequence of (relative-path, file-bytes)), NOT over the
// container bytes. It is therefore independent of the archive format:
// the same files yield the same hash whether packed as ZIP or read back
// from a legacy tar.gz bundle. This keeps cache keys and persisted run
// hashes stable across the format migration.
//
// Returns an error when:
//   - srcDir is not a directory
//   - srcDir contains no main.bot at root
//   - any entry is a symlink, device, or non-regular file
//   - outPath already exists (use --force at the CLI layer to overwrite)
func PackDir(srcDir, outPath string) (*PackResult, error) {
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, fmt.Errorf("bundle/pack: resolve src %s: %w", srcDir, err)
	}

	// Validate the bundle layout up front (cheap, fail-fast).
	hasBot := false
	for _, name := range botFileNames {
		if _, err := os.Stat(filepath.Join(absSrc, name)); err == nil {
			hasBot = true
			break
		}
	}
	if !hasBot {
		return nil, fmt.Errorf("bundle/pack: %s contains no main.bot at root", absSrc)
	}

	return PackTree(absSrc, outPath)
}

// PackTree is PackDir without the bot-bundle layout requirement: it packs
// ANY directory tree into the same deterministic ZIP (sorted entries,
// pinned timestamps, symlinks refused, same skip rules and content hash).
// Used for non-bot archives — e.g. the marketplace serving a plugin's
// source tree as a downloadable ZIP.
func PackTree(srcDir, outPath string) (*PackResult, error) {
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, fmt.Errorf("bundle/pack: resolve src %s: %w", srcDir, err)
	}
	info, err := os.Stat(absSrc)
	if err != nil {
		return nil, fmt.Errorf("bundle/pack: stat %s: %w", absSrc, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle/pack: %s is not a directory", absSrc)
	}

	absOut, err := filepath.Abs(outPath)
	if err != nil {
		return nil, fmt.Errorf("bundle/pack: resolve out %s: %w", outPath, err)
	}
	if _, err := os.Stat(absOut); err == nil {
		return nil, fmt.Errorf("bundle/pack: output %s already exists", absOut)
	}
	if _, err := os.Stat(filepath.Dir(absOut)); err != nil {
		return nil, fmt.Errorf("bundle/pack: parent directory of %s does not exist (mkdir -p?)", absOut)
	}

	// Collect entries deterministically: walk, filter, sort.
	entries, totalBytes, err := collectEntries(absSrc)
	if err != nil {
		return nil, err
	}

	out, err := os.OpenFile(absOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("bundle/pack: create %s: %w", absOut, err)
	}
	zw := zip.NewWriter(out)

	hasher := newContentHasher()

	for _, e := range entries {
		if err := writeZipEntry(zw, hasher, e); err != nil {
			_ = zw.Close()
			_ = out.Close()
			_ = os.Remove(absOut)
			return nil, err
		}
	}

	if err := zw.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(absOut)
		return nil, fmt.Errorf("bundle/pack: close zip: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(absOut)
		return nil, fmt.Errorf("bundle/pack: close %s: %w", absOut, err)
	}

	outInfo, err := os.Stat(absOut)
	if err != nil {
		return nil, fmt.Errorf("bundle/pack: stat output: %w", err)
	}
	return &PackResult{
		OutputPath: absOut,
		Hash:       hex.EncodeToString(hasher.Sum(nil)),
		Entries:    len(entries),
		BytesIn:    totalBytes,
		BytesOut:   outInfo.Size(),
	}, nil
}

// packEntry is one walker result, normalised to an archive-relative path.
type packEntry struct {
	rel     string // slash-separated relative path, no leading slash
	isDir   bool
	size    int64
	absPath string
}

func collectEntries(srcDir string) ([]packEntry, int64, error) {
	var entries []packEntry
	var totalBytes int64
	walkErr := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == srcDir {
			return nil // skip root, archive entries are children
		}
		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if shouldSkip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		// Reject anything that isn't a regular file or directory.
		// Lstat-style check via info.Mode() — d.Type() collapses irregular
		// types we explicitly want to refuse.
		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle/pack: symlinks not allowed (%s)", rel)
		}
		if !mode.IsRegular() && !d.IsDir() {
			return fmt.Errorf("bundle/pack: unsupported entry type for %s (only regular files and directories allowed)", rel)
		}
		entries = append(entries, packEntry{
			rel:     rel,
			isDir:   d.IsDir(),
			size:    info.Size(),
			absPath: path,
		})
		if !d.IsDir() {
			totalBytes += info.Size()
		}
		return nil
	})
	if walkErr != nil {
		return nil, 0, fmt.Errorf("bundle/pack: walk %s: %w", srcDir, walkErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries, totalBytes, nil
}

// writeZipEntry writes one packEntry into the ZIP and feeds its file
// content into the logical content hasher. Directory entries get a
// trailing slash (the ZIP convention) and 0755 mode; regular files get
// 0644. Every entry is stamped with the fixed zipEpoch so the archive
// bytes are reproducible.
func writeZipEntry(zw *zip.Writer, hasher *contentHasher, e packEntry) error {
	if e.isDir {
		hdr := &zip.FileHeader{
			Name:     e.rel + "/",
			Modified: zipEpoch,
		}
		hdr.SetMode(0o755 | os.ModeDir)
		if _, err := zw.CreateHeader(hdr); err != nil {
			return fmt.Errorf("bundle/pack: write header %s: %w", e.rel, err)
		}
		return nil
	}
	hdr := &zip.FileHeader{
		Name:     e.rel,
		Method:   zip.Deflate,
		Modified: zipEpoch,
	}
	hdr.SetMode(0o644)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return fmt.Errorf("bundle/pack: write header %s: %w", e.rel, err)
	}
	f, err := os.Open(e.absPath)
	if err != nil {
		return fmt.Errorf("bundle/pack: open %s: %w", e.absPath, err)
	}
	defer f.Close()
	// Fold this file into the logical content hash (path then bytes),
	// then copy the bytes into the ZIP entry.
	hasher.AddFile(e.rel)
	n, copyErr := io.Copy(io.MultiWriter(w, hasher), f)
	if copyErr != nil {
		return fmt.Errorf("bundle/pack: write body %s: %w", e.rel, copyErr)
	}
	if n != e.size {
		return fmt.Errorf("bundle/pack: short write for %s (wrote %d, expected %d — file changed during pack?)", e.rel, n, e.size)
	}
	return nil
}

// shouldSkip reports whether a relative path matches a pack-time
// ignore rule. Operates on the slash-form path so all OSes behave
// the same way.
func shouldSkip(rel string) bool {
	for _, p := range skipPatterns {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
		if base := filepath.Base(rel); base == p {
			return true
		}
	}
	for _, suf := range skipSuffixes {
		if strings.HasSuffix(rel, suf) {
			return true
		}
	}
	return false
}
