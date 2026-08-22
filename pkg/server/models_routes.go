package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/modelcatalog"
)

// handleModels serves the model registry: every model iterion knows about,
// crossed with the credentials THIS host holds, the capabilities each model
// has, and what it costs.
//
// It exists because the studio's model field was free text with a datalist of
// hints — an operator could not see what they had access to, what it cost, or
// whether it could even call tools. The answer is host state, not a hardcoded
// list, so it is computed per request off the same cached detect.Report that
// backs /api/backends/detect.
//
// Query params:
//
//	spec=provider/model  repeatable; ADDS to the known set (so a caller can ask
//	                     about a model the curated list omits — e.g. a node's
//	                     own DSL default — without narrowing the picker to the
//	                     models already in use, which is the one list from
//	                     which no new choice can be made)
//	refresh=1            re-probe host credentials AND re-fetch the model-spec
//	                     aggregator before answering
//
// Read-only, and reveals only capability + credential SOURCE names
// (e.g. "ANTHROPIC_API_KEY") — never a credential value.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.detectorOnce.Do(func() {
		s.detector = detect.NewCachedDetector(backendDetectTTL)
	})

	refresh := r.URL.Query().Get("refresh") == "1"
	if refresh {
		// Same ordering as handleBackendsDetect: the hook may (un)set env
		// vars, so it has to fire before the cache is dropped.
		if s.OnForceRefresh != nil {
			s.OnForceRefresh()
		}
		s.detector.Invalidate()
	}
	report := s.detector.Get(r.Context())

	cat, err := modelcatalog.Build(r.Context(), modelcatalog.Options{
		ExtraSpecs: dedupeSpecs(r.URL.Query()["spec"]),
		Refresh:    refresh,
		Report:     &report,
	})
	if err != nil {
		// Defensive: the handler only ever passes ExtraSpecs, which Build
		// degrades into cat.InvalidSpecs rather than raising. A malformed
		// ?spec= must not 400 the whole registry — LaunchView asks about
		// every node's DSL default at once, so one bot pinning a bad
		// `model:` would otherwise render an empty picker for every model
		// on the host.
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	s.writeJSONFor(w, r, cat)
}

// dedupeSpecs trims, drops empties and removes duplicates while preserving the
// caller's order. LaunchView asks about every LLM node's default at once, and
// a bot with twenty nodes on one model should not produce twenty rows.
func dedupeSpecs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		// Repeated ?spec= params and a single comma-separated one are both
		// natural to write; accept either rather than 400 on the wrong guess.
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
