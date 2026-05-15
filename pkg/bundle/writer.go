package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// epochZero is the canonical timestamp written into every tar/gzip
// header so the output is reproducible. We use the IEEE 1003.1-1988
// "no timestamp" sentinel (Unix epoch 0); gzip special-cases it as
// "no timestamp available" and tar writes it as all-zero octal.
var epochZero = time.Unix(0, 0).UTC()

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

// PackResult summarises a successful PackDir invocation.
type PackResult struct {
	OutputPath string // absolute path of the .botz file
	Hash       string // SHA-256 of the uncompressed tar stream — matches Bundle.Hash on Open
	Entries    int    // number of tar entries written (files + directories)
	BytesIn    int64  // sum of uncompressed file bytes
	BytesOut   int64  // size of the .botz on disk
}

// PackDir creates a .botz tar.gz archive at outPath from the contents
// of srcDir. The bundle layout is the same as accepted by [Open] /
// [OpenDir]: bot.iter (or bot.bot) at the root, plus optional
// manifest.yaml, skills/, prompts/, attachments/.
//
// The archive is deterministic — entries are sorted alphabetically,
// timestamps zeroed, ownership stripped, modes uniformly set — so two
// PackDir invocations on the same directory tree produce byte-identical
// output. This is essential for cache-key stability: the hash of the
// uncompressed tar matches between producer and consumer machines.
//
// Returns an error when:
//   - srcDir is not a directory
//   - srcDir contains no bot.iter or bot.bot at root
//   - any entry is a symlink, device, or non-regular file
//   - outPath already exists (use --force at the CLI layer to overwrite)
func PackDir(srcDir, outPath string) (*PackResult, error) {
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

	// Validate the bundle layout up front (cheap, fail-fast).
	hasBot := false
	for _, name := range botFileNames {
		if _, err := os.Stat(filepath.Join(absSrc, name)); err == nil {
			hasBot = true
			break
		}
	}
	if !hasBot {
		return nil, fmt.Errorf("bundle/pack: %s contains no bot.iter or bot.bot at root", absSrc)
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
	gz := gzip.NewWriter(out)
	// Strip non-deterministic gzip header fields so the compressed
	// bytes are stable across machines (date, OS, filename).
	gz.ModTime = epochZero
	gz.Name = ""
	gz.Comment = ""
	gz.OS = 255 // "unknown" — most-portable sentinel

	hasher := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(gz, hasher))

	for _, e := range entries {
		if err := writeTarEntry(tw, absSrc, e); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			_ = out.Close()
			_ = os.Remove(absOut)
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = out.Close()
		_ = os.Remove(absOut)
		return nil, fmt.Errorf("bundle/pack: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(absOut)
		return nil, fmt.Errorf("bundle/pack: close gzip: %w", err)
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

// packEntry is one walker result, normalised to a tar-relative path.
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
			return nil // skip root, tar entries are children
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

func writeTarEntry(tw *tar.Writer, srcDir string, e packEntry) error {
	hdr := &tar.Header{
		Name:    e.rel,
		ModTime: epochZero,
		Format:  tar.FormatUSTAR,
	}
	if e.isDir {
		hdr.Typeflag = tar.TypeDir
		hdr.Mode = 0o755
		hdr.Name = e.rel + "/"
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("bundle/pack: write header %s: %w", e.rel, err)
		}
		return nil
	}
	hdr.Typeflag = tar.TypeReg
	hdr.Mode = 0o644
	hdr.Size = e.size
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("bundle/pack: write header %s: %w", e.rel, err)
	}
	f, err := os.Open(e.absPath)
	if err != nil {
		return fmt.Errorf("bundle/pack: open %s: %w", e.absPath, err)
	}
	defer f.Close()
	n, copyErr := io.Copy(tw, f)
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

