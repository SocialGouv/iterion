package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/brand"
)

// registerBrandRoutes serves the iterion-bot mascot embedded in pkg/brand at a
// stable public URL (/brand/iterion-bot.png, /brand/iterion-bot-circle.png).
// The studio's download links for the uploads iterion cannot perform itself
// (a GitHub App's logo has no API) and the docs point here, so the bytes
// exist in one place. Public by construction: isPublicPath admits every
// non-/api/ path.
func (s *Server) registerBrandRoutes() {
	s.mux.HandleFunc("GET /brand/{file}", handleBrandAsset)
}

// ServeBrandAsset answers one /brand/<file> request.
//
// Exported for the same reason NotFoundBuildAsset next door is: every surface
// that serves these bytes needs the IDENTICAL answer, and the desktop asset
// proxy lives in its own package. It had a copy for one round, and the copy
// had already diverged on three things a reader would not notice — no ETag,
// so every revalidation re-downloaded 79–158 KB; a text/plain 404 where this
// one is JSON; and no If-None-Match handling, so a 304 came back as a full
// body.
func handleBrandAsset(w http.ResponseWriter, r *http.Request) {
	ServeBrandAsset(w, r, r.PathValue("file"))
}

func ServeBrandAsset(w http.ResponseWriter, r *http.Request, file string) {
	v, ok := brand.VariantForFilename(file)
	if !ok {
		httpError(w, http.StatusNotFound, "no such brand asset: %s", file)
		return
	}
	data := brand.BotAvatar(v)
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if etagSatisfied(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// etagSatisfied applies RFC 9110 §13.1.2 to an If-None-Match header: a
// comma-separated list, `*`, and the weak comparison — an ingress that gzips
// rewrites a strong tag to W/"…", and an exact compare would make every
// browser revalidation download the asset again.
func etagSatisfied(ifNoneMatch, etag string) bool {
	for _, c := range strings.Split(ifNoneMatch, ",") {
		c = strings.TrimSpace(c)
		if c == "*" || strings.TrimPrefix(c, "W/") == etag {
			return true
		}
	}
	return false
}
