package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/marketplace"
)

// marketplaceDownloadCacheOnce lazily creates the on-disk cache root the
// first time a download is served, so a server that never serves a
// download pays nothing. mpCacheRoot is the resolved directory.
var (
	mpCacheOnce sync.Once
	mpCacheRoot string
	mpCacheErr  error
	// mpKeyLocks serialises concurrent downloads of the same cache key so
	// two requests don't race to clone + pack the same bundle. One mutex
	// per key (the bundle is small; the map is bounded by distinct
	// slug@version pairs).
	mpKeyLocks sync.Map // string -> *sync.Mutex
)

func mpCacheDir() (string, error) {
	mpCacheOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "iterion-marketplace-cache")
		mpCacheErr = os.MkdirAll(dir, 0o755)
		mpCacheRoot = dir
	})
	return mpCacheRoot, mpCacheErr
}

func mpKeyLock(key string) *sync.Mutex {
	m, _ := mpKeyLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// handleMarketplaceDownload answers GET /api/v1/marketplace/bots/{slug}/download.
// It streams the bot's bundle as a `.botz` archive, materialising it on
// demand from the entry's source coordinates (a git clone, a local
// builtin path, …) and packing it with bundle.PackDir. Public-readable
// (see isPublicMarketplaceRead) — a viewer only reaches a bundle they may
// see (Visible gate, identical to handleMarketplaceGet). Results are
// cached on disk by slug@version since the clone+pack is expensive and a
// published version is immutable.
func (s *Server) handleMarketplaceDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: slug required")
		return
	}
	entry, ok, err := s.marketplace.Get(r.Context(), slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: get: %v", err)
		return
	}
	if !ok || !marketplace.Visible(*entry, s.marketplaceViewer(r)) {
		// 404 (not 403) so a scoped/pending slug's existence never leaks.
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: %q not found", slug)
		return
	}
	// A plugin has no .botz form; it installs via the plugin pipeline.
	if marketplace.EffectiveKind(*entry) == marketplace.KindPlugin {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: %q is a plugin entry — install it with `iterion plugin install %s`", slug, entry.RepoURL)
		return
	}

	src := strings.TrimSpace(entry.RepoURL)
	if src == "" {
		// Upload-sourced entries park their bytes in BundleRef, a backend
		// that isn't wired yet; nothing else gives us a downloadable tree.
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: %q has no downloadable bundle", slug)
		return
	}

	botzPath, err := s.materializeBotz(r, *entry)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadGateway, "marketplace: build bundle: %v", err)
		return
	}

	f, err := os.Open(botzPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: open bundle: %v", err)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: stat bundle: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", slug+".botz"))
	// http.ServeContent sets Content-Length, handles Range + If-Modified.
	http.ServeContent(w, r, slug+".botz", info.ModTime(), f)
}

// materializeBotz returns the filesystem path of a packed `.botz` for the
// entry, building it (and caching it) on first request. The cache key is
// the entry's identity + version + source coordinates, so a re-published
// version (or a moved repo) busts the cache. An entry with no version
// (e.g. a manifest that omits it) is packed fresh every time — correctness
// over the cache hit.
func (s *Server) materializeBotz(r *http.Request, entry marketplace.Entry) (string, error) {
	cacheable := strings.TrimSpace(entry.Version) != ""
	keyRaw := strings.Join([]string{entry.Slug, entry.Version, entry.RepoURL, entry.Ref, entry.Subpath}, "\x00")
	sum := sha256.Sum256([]byte(keyRaw))
	key := hex.EncodeToString(sum[:])

	root, err := mpCacheDir()
	if err != nil {
		return "", err
	}

	lock := mpKeyLock(key)
	lock.Lock()
	defer lock.Unlock()

	cachePath := filepath.Join(root, key+".botz")
	if cacheable {
		if _, statErr := os.Stat(cachePath); statErr == nil {
			return cachePath, nil
		}
	}

	// Materialize the bundle dir (git clone or local builtin path) read-only.
	dir, cleanup, err := botinstall.Fetch(r.Context(), botinstall.Options{
		Source: entry.RepoURL,
		Ref:    entry.Ref,
		Path:   entry.Subpath,
	})
	if err != nil {
		return "", err
	}
	defer cleanup()

	// Pack into a unique temp file first (PackDir refuses an existing
	// outPath), then rename into place so a concurrent reader never sees a
	// half-written archive.
	tmpOut := filepath.Join(root, key+".tmp-"+randSuffixFromKey(key, entry.UpdatedAt))
	_ = os.Remove(tmpOut)
	if _, err := bundle.PackDir(dir, tmpOut); err != nil {
		_ = os.Remove(tmpOut)
		return "", err
	}
	if !cacheable {
		// Caller streams it; leave it for the OS temp reaper. Returning the
		// throwaway path keeps the non-cached path simple.
		return tmpOut, nil
	}
	if err := os.Rename(tmpOut, cachePath); err != nil {
		_ = os.Remove(tmpOut)
		return "", err
	}
	return cachePath, nil
}

// randSuffixFromKey derives a stable-but-unique temp suffix without the
// banned Math.random/Date.now equivalents — the cache key plus the entry's
// UpdatedAt timestamp is enough entropy to avoid collisions between two
// distinct entries racing to pack, and the per-key lock already serialises
// the same entry.
func randSuffixFromKey(key, salt string) string {
	sum := sha256.Sum256([]byte(key + "|" + salt))
	return hex.EncodeToString(sum[:6])
}
