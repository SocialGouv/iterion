package server

import (
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
)

// backendDetectTTL is how long the detect Report stays cached. Detection
// is cheap (env reads + filesystem stats) but the editor calls
// /api/backends/detect on every mount and could re-poll on focus, so we
// don't want to re-stat on every keystroke either.
const backendDetectTTL = 30 * time.Second

// handleBackendsDetect serves a snapshot of available LLM backends and
// providers. The route is read-only and reveals only booleans + source
// names (e.g. "ANTHROPIC_API_KEY") — never the credential values.
func (s *Server) handleBackendsDetect(w http.ResponseWriter, r *http.Request) {
	s.detectorOnce.Do(func() {
		s.detector = detect.NewCachedDetector(backendDetectTTL)
	})
	writeJSON(w, s.detector.Get(r.Context()))
}
