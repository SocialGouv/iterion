package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	apikit "github.com/SocialGouv/claw-code-go/pkg/apikit"
	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// effortCapabilitiesResponse is the JSON shape returned by
// GET /api/effort-capabilities.
type effortCapabilitiesResponse struct {
	// Supported is the ordered list of reasoning_effort values accepted
	// by the (backend, model) pair, low→high. Empty when the model does
	// not declare support — clients should treat that as "hide the
	// reasoning_effort field for this model".
	Supported []string `json:"supported"`

	// Default is the value the provider uses when no effort is sent.
	// Empty string when the provider has no documented default.
	Default string `json:"default"`

	// Source identifies where the data came from, for diagnostics:
	//   - "claw-registry" : claw-code-go's curated model registry
	//   - "codex-cli"     : live response from the Codex CLI ListModels
	//   - "codex-fallback": Codex CLI unreachable; static SDK constants
	Source string `json:"source"`
}

// codexEffortFallback is the static list emitted when the Codex CLI is
// unavailable. Mirrors the codex SDK's Effort constants (low/medium/high/max).
var codexEffortFallback = []string{"low", "medium", "high", "max"}

// codexCacheTTL is how long a Codex ListModels response is reused before
// re-querying the CLI. Codex doesn't change models mid-session in
// practice, so a lazy cache avoids spawning a CLI process per request.
const codexCacheTTL = 10 * time.Minute

type codexCacheEntry struct {
	models    []codexsdk.ModelInfo
	fetchedAt time.Time
}

var (
	codexCacheMu sync.Mutex
	codexCache   *codexCacheEntry
)

// fetchCodexModels returns the cached Codex model list, refreshing it
// when stale or absent. Returns (nil, err) on first-time CLI failures so
// callers can fall back to the static list.
func fetchCodexModels(ctx context.Context) ([]codexsdk.ModelInfo, error) {
	codexCacheMu.Lock()
	defer codexCacheMu.Unlock()
	if codexCache != nil && time.Since(codexCache.fetchedAt) < codexCacheTTL {
		return codexCache.models, nil
	}
	models, err := codexsdk.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	codexCache = &codexCacheEntry{models: models, fetchedAt: time.Now()}
	return models, nil
}

// codexCapabilities resolves the effort matrix for a Codex model by name.
// Falls back to codexEffortFallback when the CLI is unreachable or the
// model is not listed.
func codexCapabilities(ctx context.Context, model string) (effortCapabilitiesResponse, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	models, err := fetchCodexModels(queryCtx)
	if err != nil {
		return effortCapabilitiesResponse{
			Supported: codexEffortFallback,
			Source:    "codex-fallback",
		}, nil
	}

	for _, m := range models {
		if m.ID != model && m.Model != model {
			continue
		}
		supported := make([]string, 0, len(m.SupportedReasoningEfforts))
		for _, opt := range m.SupportedReasoningEfforts {
			supported = append(supported, opt.Value)
		}
		return effortCapabilitiesResponse{
			Supported: supported,
			Default:   m.DefaultReasoningEffort,
			Source:    "codex-cli",
		}, nil
	}

	// Model not in the live list — return fallback rather than empty so
	// the editor still shows something sensible.
	return effortCapabilitiesResponse{
		Supported: codexEffortFallback,
		Source:    "codex-fallback",
	}, nil
}

// resolveEffortResponse is the JSON shape returned by
// GET /api/resolve-effort. Lets the editor canvas display the
// resolved value (e.g., "max") for env-substituted literals like
// "${VIBE_EFFORT:-max}" without exposing process env over HTTP.
type resolveEffortResponse struct {
	// Resolved is the validated effort level after env substitution,
	// or "" if the literal is empty / expansion produced an invalid
	// value. Callers should fall back to displaying the literal in
	// that case.
	Resolved string `json:"resolved"`
}

// handleResolveEffort answers
//
//	GET /api/resolve-effort?literal=<effort-literal>
//
// with the env-resolved effort level for the supplied literal. The
// canonical use case is "${VAR:-default}" / "${VAR}" forms in
// reasoning_effort fields — the editor canvas reads these from the
// AST and asks the server to expand them so the rendered bar matches
// the runtime behaviour.
func (s *Server) handleResolveEffort(w http.ResponseWriter, r *http.Request) {
	literal := r.URL.Query().Get("literal")
	writeJSON(w, resolveEffortResponse{Resolved: ir.ResolveEffortLiteral(literal)})
}

// handleEffortCapabilities answers
//
//	GET /api/effort-capabilities?backend=<name>&model=<canonical-or-alias>
//
// with the supported reasoning_effort levels for that pair. Both
// parameters are required. Unknown backends produce 400.
func (s *Server) handleEffortCapabilities(w http.ResponseWriter, r *http.Request) {
	backend := r.URL.Query().Get("backend")
	model := r.URL.Query().Get("model")
	if backend == "" {
		httpError(w, http.StatusBadRequest, "missing required query param: backend")
		return
	}
	if model == "" {
		httpError(w, http.StatusBadRequest, "missing required query param: model")
		return
	}

	switch backend {
	case "claude_code", "claw":
		supported, def := apikit.EffortCapabilities(model)
		writeJSON(w, effortCapabilitiesResponse{
			Supported: supported,
			Default:   def,
			Source:    "claw-registry",
		})
	case "codex":
		resp, err := codexCapabilities(r.Context(), model)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "codex lookup failed: %v", err)
			return
		}
		writeJSON(w, resp)
	default:
		httpError(w, http.StatusBadRequest, "unknown backend %q", backend)
	}
}
