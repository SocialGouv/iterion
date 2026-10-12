package server

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	apikit "github.com/SocialGouv/claw-code-go/pkg/apikit"
	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
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
// unavailable. Mirrors the codex SDK's Effort constants every codex model
// accepts (low/medium/high/max — the SDK's "minimal" has no iterion
// spelling). "none" is NOT in this list: only known none-carriers get it,
// see codexEffortFallbackFor.
var codexEffortFallback = []string{"low", "medium", "high", "max"}

// codexModelCarriesNone reports whether the model is a known none-carrier.
// The claw registry (apikit) is the catalogue of record for which models
// carry none — and it agrees with the runtime: codex accepts none for
// GPT-6 Sol/Luna (and 400s it elsewhere) even though the CLI's model/list
// response omits the level (verified on codex 0.156.1).
func codexModelCarriesNone(model string) bool {
	supported, _ := apikit.EffortCapabilities(model)
	return slices.Contains(supported, "none")
}

// codexEffortFallbackFor is codexEffortFallback plus "none" when the model
// is a known none-carrier. The live list comes from the CLI's per-model
// capabilities; the fallback path exists precisely because that CLI is
// unreachable, so the per-model truth it carries is unavailable — and
// offering none for every codex model would promise a level the CLI refuses
// at run time.
func codexEffortFallbackFor(model string) []string {
	if codexModelCarriesNone(model) {
		return append([]string{"none"}, codexEffortFallback...)
	}
	return codexEffortFallback
}

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
// Falls back to codexEffortFallbackFor when the CLI is unreachable or the
// model is not listed.
func codexCapabilities(ctx context.Context, model string) (effortCapabilitiesResponse, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	models, err := fetchCodexModels(queryCtx)
	if err != nil {
		return effortCapabilitiesResponse{
			Supported: codexEffortFallbackFor(model),
			Source:    "codex-fallback",
		}, nil
	}

	for _, m := range models {
		if m.ID != model && m.Model != model {
			continue
		}
		supported := make([]string, 0, len(m.SupportedReasoningEfforts)+1)
		for _, opt := range m.SupportedReasoningEfforts {
			supported = append(supported, opt.Value)
		}
		// The CLI's model/list omits none even for the models that accept
		// it (codex 0.156.1); union it in from the catalogue of record so
		// the live path and the fallback agree with the runtime.
		if codexModelCarriesNone(model) && !slices.Contains(supported, "none") {
			supported = append([]string{"none"}, supported...)
		}
		return effortCapabilitiesResponse{
			Supported: supported,
			Default:   m.DefaultReasoningEffort,
			Source:    "codex-cli",
		}, nil
	}

	// Model not in the live list — return fallback rather than empty so
	// the studio still shows something sensible.
	return effortCapabilitiesResponse{
		Supported: codexEffortFallbackFor(model),
		Source:    "codex-fallback",
	}, nil
}

// resolveEffortResponse is the JSON shape returned by
// GET /api/resolve-effort. Lets the studio canvas display the
// resolved value (e.g., "max") for env-substituted literals like
// "${VIBE_EFFORT:-max}" without exposing process env over HTTP.
type resolveEffortResponse struct {
	// Resolved is the validated effort level after env substitution,
	// or "" if the literal is empty / expansion produced an invalid
	// value. Callers should fall back to displaying the literal in
	// that case.
	Resolved string `json:"resolved"`
}

// maxResolveLiteralBytes caps GET /api/resolve-{model,effort} `literal`
// query values. A model spec is <128 bytes; 512 is ample headroom for
// a ${VAR:-default} form and rejects dump-the-environment probes.
const maxResolveLiteralBytes = 512

// handleResolveEffort answers
//
//	GET /api/resolve-effort?literal=<effort-literal>
//
// with the env-resolved effort level for the supplied literal. The
// canonical use case is "${VAR:-default}" / "${VAR}" forms in
// reasoning_effort fields — the studio canvas reads these from the
// AST and asks the server to expand them so the rendered bar matches
// the runtime behaviour.
//
// Expansion is against THIS process's env (the studio / API server
// pod), not the runner that will execute the bot. The canvas is a
// preview of this process, not a promise of the runner.
func (s *Server) handleResolveEffort(w http.ResponseWriter, r *http.Request) {
	literal := r.URL.Query().Get("literal")
	if len(literal) > maxResolveLiteralBytes {
		writeJSON(w, resolveEffortResponse{})
		return
	}
	writeJSON(w, resolveEffortResponse{Resolved: ir.ResolveEffortLiteral(literal)})
}

// resolveModelResponse is the JSON shape returned by GET /api/resolve-model.
// Lets the studio canvas display the resolved model id (e.g. "gpt-5.6-sol")
// for env-substituted literals like "${CODEX_MODEL:-openai-codex/gpt-5.6-sol}"
// without exposing process env over HTTP — expansions that do not look
// like a model spec come back empty.
type resolveModelResponse struct {
	Resolved string `json:"resolved"`
}

// handleResolveModel answers
//
//	GET /api/resolve-model?literal=<model-literal>
//
// with the env-resolved model spec for the supplied literal. The
// canonical use case is "${VAR:-default}" / "${VAR}" forms in agent
// `model:` fields — the studio canvas reads these from the AST and
// asks the server to expand them so the card shows sol/terra/luna
// (or whatever the process env actually selected) instead of "env".
//
// Expansion is against THIS process's env (the studio / API server
// pod), not the runner that will execute the bot. The canvas is a
// preview of this process, not a promise of the runner — same
// contract as /api/resolve-effort. Only env vars whose names contain
// "MODEL" and do not look like a credential are expanded (see
// ir.ResolveModelLiteral); nested ${${X}} forms come back empty.
func (s *Server) handleResolveModel(w http.ResponseWriter, r *http.Request) {
	literal := r.URL.Query().Get("literal")
	if len(literal) > maxResolveLiteralBytes {
		writeJSON(w, resolveModelResponse{})
		return
	}
	writeJSON(w, resolveModelResponse{Resolved: ir.ResolveModelLiteral(literal)})
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
		// The registry knows vendor ids, never a routing prefix: a node's
		// spec ("anthropic/claude-opus-5-5") is looked up on its capability
		// id, the id claw clamps the node's effort on.
		supported, def := apikit.EffortCapabilities(modelroute.Parse(model).CapabilityID())
		// Surface the "ultracode" mode (xhigh + workflow-orchestration
		// prerogative) only on the models that carry its orchestration half
		// (Opus 4.8, the Claude 5 family) — the same predicate the compiler's
		// C089 gate uses, so the studio and the compiler never disagree.
		// ResolveModelAlias maps the "opus" alias to the canonical id and
		// returns any literal unchanged. See docs/ultracode.md.
		if ir.ModelSupportsUltracode(apikit.ResolveModelAlias(model)) {
			supported = append(append([]string(nil), supported...), "ultracode")
		}
		writeJSON(w, effortCapabilitiesResponse{
			Supported: supported,
			Default:   def,
			Source:    "claw-registry",
		})
	case "pi":
		// pi's own dial is off|minimal|low|medium|high|xhigh|max — a strict
		// superset of iterion's, minus the level iterion has no way to
		// express (minimal; iterion's none maps onto pi's off — see
		// piMapEffort). It is model-independent: pi maps the level onto each
		// provider's own thinking budget, so there is nothing to look up.
		writeJSON(w, effortCapabilitiesResponse{
			Supported: []string{"none", "low", "medium", "high", "xhigh", "max"},
			Default:   "medium",
			Source:    "pi-thinking",
		})
	case "opencode":
		// opencode's dial is `--variant`, and the accepted names are
		// per-MODEL: it derives them from the model's own reasoning
		// options, so there is no static set to look up. These are the
		// levels iterion PASSES THROUGH verbatim (xhigh and ultracode
		// collapse onto high before argv, so offering them here would
		// promise a level the backend silently substitutes). Measured on
		// opencode 1.1.19: a variant the model does not carry is dropped in
		// silence, so offering a level is never a promise it took effect.
		// `none` is deliberately absent although opencodeMapEffort passes it
		// through: whether a model carries a "none" variant is exactly the
		// per-model fact iterion cannot enumerate, and the silent drop would
		// turn the offer into an off-switch that quietly no-ops — the same
		// caution that gates none to known carriers on the codex arm.
		writeJSON(w, effortCapabilitiesResponse{
			Supported: []string{"low", "medium", "high", "max"},
			// No documented default: with no --variant, the model's own
			// setting applies and iterion has nothing to name.
			Default: "",
			Source:  "opencode-variant",
		})
	case "grok":
		// grok's dial is `--reasoning-effort`, passed through verbatim for
		// every iterion level except ultracode, which collapses onto high
		// before argv (Grok has no ultracode mode — see grokMapEffort), so
		// offering ultracode here would promise a level the backend silently
		// substitutes. Model-independent: the CLI takes the same flag for
		// every model, so there is nothing to look up. The CLI's own accepted
		// set is unmeasured — same caveat as the opencode arm. none IS
		// offered here, unlike that arm: opencode's variants are a per-model
		// catalogue whose uncarried names drop in silence, so an off-switch
		// promise could quietly no-op there; grok's dial is a single
		// model-independent flag, so the whole list iterion can pass is
		// offered and the unmeasured caveat above covers the rest.
		writeJSON(w, effortCapabilitiesResponse{
			Supported: []string{"none", "low", "medium", "high", "xhigh", "max"},
			// No documented default: with no --reasoning-effort the CLI's own
			// setting applies and iterion has nothing to name.
			Default: "",
			Source:  "grok-reasoning-effort",
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
