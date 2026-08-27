package botsource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Materialize writes a bundle file map under dir, creating it if needed.
// It is ALL-OR-NOTHING: the first unsafe path, mkdir or write failure
// aborts with an explicit error and removes everything written under dir,
// so a caller can never operate on a silently partial tree (a bundle
// missing one skill still "works" while doing the wrong thing). Every key
// is re-validated against the same traversal rules Validate enforces at
// write time — defense in depth for a row persisted by an older build.
func Materialize(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("botsource: materialize %s: %w", dir, err)
	}
	for rel, content := range files {
		if err := safeBundlePath(rel); err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("botsource: materialize %s: %w", rel, err)
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("botsource: materialize %s: %w", rel, err)
		}
	}
	return nil
}

// ReadBundleDir is Materialize's inverse: it walks a bundle directory into
// the path→content map the store persists. One definition of "what a
// bundle dir contains" shared by the CLI push and the server-side
// fork-from-catalog, so the two cannot drift: skips .git/ and Go test
// files, refuses non-UTF-8 content explicitly (the store carries JSON
// text — a binary file would be corrupted, not stored).
func ReadBundleDir(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			// Generated / artifact trees that live INSIDE real bundle dirs:
			// devbox regenerates .devbox/ next to a bot's devbox.json, and a
			// dogfood run leaves .iterion/ run state — pushing either ships
			// run inputs into the deployment-wide store (or fails on the
			// first non-UTF-8 blob, naming a file the operator never meant
			// to push).
			case ".git", ".devbox", ".iterion", "node_modules", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		b, berr := os.ReadFile(p)
		if berr != nil {
			return berr
		}
		if !utf8.Valid(b) {
			return fmt.Errorf("botsource: %s is not UTF-8 text — binary files cannot be stored in a bot source (the baked bundle keeps serving it)", rel)
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// ExecutableFiles lists the bundle-relative paths (same walk + skip rules
// as ReadBundleDir) whose mode carries an execute bit. The path→content
// map drops file modes — Materialize writes everything 0o644 — so a bundle
// shipping an executable helper round-trips with the +x bit gone and a
// tool node invoking it fails on permission denied. Push surfaces warn
// from this list instead of failing silently at run time.
func ExecutableFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".devbox", ".iterion", "node_modules", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil //nolint:nilerr // advisory walk: an unreadable entry just isn't listed
		}
		if info.Mode()&0o111 != 0 {
			if rel, rerr := filepath.Rel(dir, p); rerr == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	return out
}

// Digest returns the sha256 hex digest of the bundle content, computed
// over the sorted (path, content) pairs — the provenance record the audit
// log and `admin bots show` carry so "what exactly is deployed" has a
// stable, comparable answer.
func Digest(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		// Length-prefix both fields so (path, content) pairs cannot collide
		// across boundaries.
		fmt.Fprintf(h, "%d:%s%d:", len(p), p, len(files[p]))
		h.Write([]byte(files[p]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IsPlatform reports whether a tenant id is the platform sentinel.
func IsPlatform(tenantID string) bool {
	return strings.TrimSpace(tenantID) == PlatformTenantID
}
