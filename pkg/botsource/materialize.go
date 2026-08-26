package botsource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
