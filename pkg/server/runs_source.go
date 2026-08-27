package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveWorkflowPath returns the absolute path the engine should
// associate with a launch / resume / answer call.
//
// Resolution rules:
//   - cloud mode + source set: return filePath as a logical label;
//     the publisher carries Source inline to the runner pod.
//   - local mode + source set: the SPA bundles the studio buffer
//     alongside file_path (e.g. for imported / freshly-saved
//     recipes — see studio/src/components/Toolbar/Toolbar.tsx).
//     The downstream subprocess (`iterion run <path>`) reads from
//     disk, so a relative basename relative to the desktop
//     process cwd would ENOENT. We materialise Source into a
//     stable per-store cache and return that absolute path.
//   - local mode without source: run through safePath as before;
//     on miss, fall back to embedded recipes shipped with the
//     binary (see materializeEmbeddedRecipe).
func (s *Server) resolveWorkflowPath(filePath, source string) (string, error) {
	if source != "" {
		if s.cfg.Mode == "cloud" {
			return filePath, nil
		}
		if materialised, ok := s.materializeInlineSource(filePath, source); ok {
			return materialised, nil
		}
		// Materialisation failed (no writable cache dir) — surface a
		// clear error rather than letting the subprocess ENOENT
		// further down the chain.
		return "", fmt.Errorf("cannot materialise inline source: no writable store/work directory configured")
	}
	abs, err := s.safePath(filePath)
	if err == nil {
		return abs, nil
	}
	// safePath rejected the input. On resume of an inline-launched run
	// (where the SPA uploaded source on launch but not on resume), the
	// persisted FilePath points at the server's inline-source cache —
	// which lives next to the run store, OUTSIDE the current WorkDir.
	// Trust paths in our own cache: the materialised file is the same
	// content the run was launched with, by construction.
	if cached, ok := s.resolveCachedInlineSource(filePath); ok {
		return cached, nil
	}
	if cached, ok := s.materializeEmbeddedRecipe(filePath); ok {
		return cached, nil
	}
	return "", err
}

// inferCatalogBotID extracts a catalog bot name from a workflow file path:
// "bots/whats-next/main.bot", "/opt/iterion/bots/whats-next/main.bot",
// "examples/foo/main.bot", "whats-next/main.bot", "whats-next", and
// "hello.bot" all map to their bundle-dir / basename. Returns "" for an
// absolute path with no bots|examples segment (an arbitrary workspace file
// that must still carry inline source in cloud). The returned id is only a
// candidate — resolveBotTiered confirms it against the tiered stores + catalog.
func inferCatalogBotID(filePath string) string {
	fp := filepath.ToSlash(strings.TrimSpace(filePath))
	if fp == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(fp, "./"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "bots" || parts[i] == "examples" {
			return parts[i+1]
		}
	}
	if filepath.IsAbs(filePath) {
		return ""
	}
	if len(parts) >= 2 && parts[len(parts)-1] == "main.bot" {
		return parts[len(parts)-2]
	}
	if len(parts) == 1 {
		return strings.TrimSuffix(parts[0], ".bot")
	}
	return ""
}

// resolveCachedInlineSource returns filePath unchanged when it points at an
// existing file under the server's inline-source cache directory. Used as a
// fallback in resolveWorkflowPath when safePath rejects an absolute path
// that the server itself wrote during a previous inline launch.
func (s *Server) resolveCachedInlineSource(filePath string) (string, bool) {
	if !filepath.IsAbs(filePath) {
		return "", false
	}
	cacheRoot := s.inlineSourceCacheDir()
	if cacheRoot == "" {
		return "", false
	}
	cacheAbs, err := filepath.Abs(cacheRoot)
	if err != nil {
		return "", false
	}
	clean := filepath.Clean(filePath)
	if !pathContains(cacheAbs, clean) {
		return "", false
	}
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() {
		return "", false
	}
	return clean, true
}

// materializeInlineSource writes the SPA-provided inline workflow content
// into a stable per-store cache directory and returns its absolute
// path. The cache lives at <storeDir>/inline-sources/<sha12>-<basename>:
//   - the file persists for the lifetime of the run store (resume,
//     inspect, report all keep working without needing the original
//     buffer to still be open in the studio);
//   - identical source content reuses the same cache file (idempotent);
//   - different content for the same basename does NOT overwrite —
//     each run's persisted FilePath uniquely identifies the bytes it
//     was launched with, so resume always replays the original source
//     even when a newer launch of the same recipe touched the cache.
//
// When filePath is empty (an studio-only buffer that was never saved on
// disk), we synthesise a basename of "inline.bot" so the cache layout
// stays predictable.
//
// Returns ok=false when no writable cache dir can be derived — the
// caller surfaces a 400 rather than letting the subprocess ENOENT.
func (s *Server) materializeInlineSource(filePath, source string) (string, bool) {
	base := filepath.Base(filePath)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "inline.bot"
	}
	cacheRoot := s.inlineSourceCacheDir()
	if cacheRoot == "" {
		return "", false
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(source))
	prefix := hex.EncodeToString(sum[:6])
	dst := filepath.Join(cacheRoot, prefix+"-"+base)
	if err := os.WriteFile(dst, []byte(source), 0o644); err != nil {
		return "", false
	}
	return dst, true
}

// inlineSourceCacheDir picks the directory under which inline-source
// recipes are materialised. Mirrors store.ResolveStoreDir's git-style
// discovery (walks up from WorkDir looking for an existing .iterion)
// so the cache lands alongside the actual run store. A divergent
// fallback would let materialisation succeed but leave the spawned
// runner subprocess unable to find the recipe (the runner resolves
// its store via the same git-style walk, so it would look at the
// ancestor .iterion, not <workDir>/.iterion).
func (s *Server) inlineSourceCacheDir() string {
	storeDir := s.resolvedStoreDir()
	if storeDir == "" {
		storeDir = filepath.Join(os.TempDir(), "iterion-inline-sources")
	}
	return filepath.Join(storeDir, "inline-sources")
}

// resolvedStoreDir returns the canonical run-store directory the
// runview Service is rooted at, mirroring the resolution rule used
// at server construction (server.go: store.ResolveStoreDir(...)).
// Empty when neither StoreDir nor WorkDir was configured (e.g. tests
// that build a Config{} directly with no FS context).
func (s *Server) resolvedStoreDir() string {
	if s.cfg.StoreDir == "" && s.cfg.WorkDir == "" {
		return ""
	}
	return store.ResolveStoreDir(s.cfg.WorkDir, s.cfg.StoreDir)
}

// materializeEmbeddedRecipe writes an embedded recipe into a stable
// per-run-store directory (one copy per binary release) and returns
// its absolute path. The lookup key is filePath as given; the caller
// passes whatever the API received, so a UI that lists recipes by
// basename ("feature-dev/main.bot" or another embedded bot path) all
// resolve correctly.
//
// We materialise rather than reading from embed.FS at execution time
// because the engine, parser, and several runtime helpers operate on
// real filesystem paths (worktree relative paths, file-watcher,
// sandbox bind-mounts). Materialisation keeps that contract intact at
// the cost of a tiny one-time disk write per recipe per run-store.
//
// Returns ok=false when the recipe is not in the embed FS, or when
// the server has no writable store dir to cache it under.
func (s *Server) materializeEmbeddedRecipe(filePath string) (string, bool) {
	data, ok := bots.Get(filePath)
	if !ok {
		return "", false
	}
	cacheRoot := s.embeddedRecipeCacheDir()
	if cacheRoot == "" {
		return "", false
	}
	dst := filepath.Join(cacheRoot, filePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", false
	}
	// Idempotent: skip the write if the cached file already matches.
	// Compare bytes, not just length — a same-length but changed recipe
	// must be rewritten, not silently served from the stale cache.
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
		return dst, true
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", false
	}
	return dst, true
}

// embeddedRecipeCacheDir returns the directory under the run store
// where embedded recipes are materialised, or "" when no store dir is
// configured (in which case embedded recipes are unavailable). Mirrors
// store.ResolveStoreDir's git-style discovery so the cache lands
// alongside the actual run store — a divergent fallback (e.g.
// <workDir>/.iterion when ResolveStoreDir picked an ancestor's
// .iterion) would create stale recipes in a directory the engine
// never reads.
func (s *Server) embeddedRecipeCacheDir() string {
	storeDir := s.resolvedStoreDir()
	if storeDir == "" {
		storeDir = filepath.Join(os.TempDir(), "iterion-embedded-recipes")
	}
	return filepath.Join(storeDir, "embedded-recipes")
}
