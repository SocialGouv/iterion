package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Dynamic model-spec registry.
//
// capabilitiesForModel() used to be a purely static heuristic table. Hardcoded
// capabilities drift as providers ship new models and revise context windows,
// so this file layers a DYNAMIC source over the curated fallback: model
// metadata (context window, max output tokens, pricing, and the
// reasoning/tool_call/temperature flags) is fetched from an online aggregator,
// cached on disk under ~/.iterion with a TTL, and merged over the curated
// defaults. The curated values in capabilities.go remain AUTHORITATIVE whenever
// the aggregator lacks a model or the fetch fails/offline — brand-new models
// (e.g. glm-5.2) are not in aggregators yet, so the curated value must win.
//
// Resolution must NEVER block or slow a run: the synchronous path does only a
// cheap disk-cache read plus map lookups. The network fetch is strictly a
// background goroutine with a short timeout. The on-disk cache makes a freshly
// fetched table available to subsequent runs/processes.

// modelSpecsSource is models.dev, picked over LiteLLM's
// model_prices_and_context_window.json because it is provider-keyed (maps
// cleanly onto iterion's "provider/modelID" spec) and exposes all three
// capability flags directly (reasoning / tool_call / temperature) alongside
// limit.context, limit.output and cost.input/output. LiteLLM lacks a
// temperature flag and has a flatter, prefix-noisy key space; it remains a
// documented fallback source but is not wired here.
const modelSpecsSource = "https://models.dev/api.json"

const (
	defaultSpecTTL     = 24 * time.Hour
	defaultSpecTimeout = 3 * time.Second
)

// fetchedSpec is the normalized, source-agnostic shape extracted from the
// aggregator for a single model. The capability flags are pointers so that
// "omitted by the source" is distinguishable from an explicit false — an
// omitted flag falls back to the curated heuristic during merge.
type fetchedSpec struct {
	ContextWindow   int     `json:"context_window,omitempty"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	InputCostPerM   float64 `json:"input_cost_per_m,omitempty"`
	OutputCostPerM  float64 `json:"output_cost_per_m,omitempty"`
	Reasoning       *bool   `json:"reasoning,omitempty"`
	ToolCall        *bool   `json:"tool_call,omitempty"`
	Temperature     *bool   `json:"temperature,omitempty"`
}

// specCacheFile is the on-disk cache layout. FetchedAt drives TTL freshness so
// the cache stays valid across copies (unlike file mtime).
type specCacheFile struct {
	FetchedAt time.Time              `json:"fetched_at"`
	Source    string                 `json:"source"`
	Specs     map[string]fetchedSpec `json:"specs"`
}

// ---------------------------------------------------------------------------
// models.dev parse structs
// ---------------------------------------------------------------------------

type mdProvider struct {
	Models map[string]mdModel `json:"models"`
}

type mdModel struct {
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Cost struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
	Reasoning   *bool `json:"reasoning"`
	ToolCall    *bool `json:"tool_call"`
	Temperature *bool `json:"temperature"`
}

// ---------------------------------------------------------------------------
// specRegistry
// ---------------------------------------------------------------------------

type specRegistry struct {
	mu        sync.Mutex
	url       string
	cachePath string
	ttl       time.Duration
	client    *http.Client
	enabled   bool // ITERION_MODEL_SPECS=off → pure curated fallback
	autoFetch bool // background refresh on stale; disabled in unit tests
	force     bool // ITERION_MODEL_SPECS_REFRESH=1 → ignore cache freshness once

	byFull    map[string]fetchedSpec // "provider/modelid" lowercased
	byModel   map[string]fetchedSpec // "modelid" lowercased
	loadedAt  time.Time              // last successful load/refresh or failed refresh attempt for TTL gating
	diskTried bool                   // disk cache lazily loaded once
	inFlight  bool                   // a background refresh is running
}

// specs is the process-wide registry consulted by capabilitiesForModel().
var specs = newSpecRegistryFromEnv()

func newSpecRegistryFromEnv() *specRegistry {
	r := &specRegistry{
		url:       modelSpecsSource,
		cachePath: defaultSpecCachePath(),
		ttl:       defaultSpecTTL,
		client:    &http.Client{Timeout: defaultSpecTimeout},
		enabled:   true,
		autoFetch: true,
		byFull:    map[string]fetchedSpec{},
		byModel:   map[string]fetchedSpec{},
	}
	// Reuse the package-wide env boolean grammar (0/false/off/no vs
	// 1/true/on/yes) so these knobs behave like ITERION_SECRETS_* etc.
	r.enabled = envFlagEnabled("ITERION_MODEL_SPECS", true)
	r.force = envFlagEnabled("ITERION_MODEL_SPECS_REFRESH", false)
	if v := strings.TrimSpace(os.Getenv("ITERION_MODEL_SPECS_URL")); v != "" {
		r.url = v
	}
	if v := strings.TrimSpace(os.Getenv("ITERION_MODEL_SPECS_CACHE")); v != "" {
		r.cachePath = v
	}
	if v := strings.TrimSpace(os.Getenv("ITERION_MODEL_SPECS_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			r.ttl = d
		}
	}
	return r
}

func defaultSpecCachePath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".iterion", "model-specs-cache.json")
	}
	// Fall back to a relative path; a failed write degrades silently anyway.
	return filepath.Join(".iterion", "model-specs-cache.json")
}

// merge overlays any fetched spec for (provider, modelID) onto the curated
// fallback. Curated wins whenever the aggregator lacks the model (the glm-5.2
// path) or dynamic resolution is disabled. This is the only method on the hot
// path; it performs no network I/O itself.
func (r *specRegistry) merge(provider, modelID string, curated ModelCapabilities) ModelCapabilities {
	if r == nil || !r.enabled {
		return curated
	}
	r.ensureFresh()
	spec, ok := r.lookup(provider, modelID)
	if !ok {
		return curated
	}
	out := curated
	// A fetched ContextWindow>0 overrides the static one.
	if spec.ContextWindow > 0 {
		out.ContextWindow = spec.ContextWindow
	}
	// Flags fall back to heuristics when the source omits them (nil pointer).
	if spec.Reasoning != nil {
		out.Reasoning = *spec.Reasoning
	}
	if spec.ToolCall != nil {
		out.ToolCall = *spec.ToolCall
	}
	if spec.Temperature != nil {
		out.Temperature = *spec.Temperature
	}
	// Pricing has no curated counterpart to fall back on, so it is carried
	// through as published: zero stays zero and means "the aggregator had no
	// price", never "free".
	out.InputCostPerM = spec.InputCostPerM
	out.OutputCostPerM = spec.OutputCostPerM
	return out
}

// lookup resolves a fetched spec by exact "provider/modelid" then by bare
// "modelid". The bare index is essential for GLM: it arrives as
// provider="anthropic", modelID="glm-5.2" (z.ai's Anthropic-compatible
// endpoint) but lives under a different provider in the aggregator.
func (r *specRegistry) lookup(provider, modelID string) (fetchedSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pl := strings.ToLower(strings.TrimSpace(provider))
	ml := strings.ToLower(strings.TrimSpace(modelID))
	if s, ok := r.byFull[pl+"/"+ml]; ok {
		return s, true
	}
	if s, ok := r.byModel[ml]; ok {
		return s, true
	}
	return fetchedSpec{}, false
}

// ensureFresh lazily loads the disk cache once, then triggers a background
// refresh when the in-memory table is stale (or absent). It never performs a
// synchronous network fetch — the caller is never blocked.
func (r *specRegistry) ensureFresh() {
	r.mu.Lock()
	if !r.diskTried {
		r.diskTried = true
		r.loadDiskCacheLocked()
	}
	stale := r.force || r.loadedAt.IsZero() || time.Since(r.loadedAt) >= r.ttl
	trigger := stale && r.autoFetch && !r.inFlight
	if trigger {
		r.inFlight = true
		// A force refresh is an explicit request to bypass initial cache
		// freshness, not permission to fetch on every hot-path call forever.
		r.force = false
	}
	r.mu.Unlock()

	if trigger {
		go func() {
			_ = r.refresh(context.Background())
		}()
	}
}

// loadDiskCacheLocked reads the on-disk cache and, when fresh (or stale but
// present — better than nothing until a refresh lands), populates the in-memory
// indices. Caller holds r.mu. Any error degrades silently to no-op.
func (r *specRegistry) loadDiskCacheLocked() {
	data, err := os.ReadFile(r.cachePath)
	if err != nil {
		return
	}
	var cf specCacheFile
	if err := json.Unmarshal(data, &cf); err != nil || len(cf.Specs) == 0 {
		return
	}
	r.indexLocked(cf.Specs)
	r.loadedAt = cf.FetchedAt
}

// refresh performs the synchronous fetch+parse+cache+swap. Every failure path
// (DNS, timeout, non-2xx, malformed JSON, write error) is swallowed so a run is
// never blocked or failed by spec resolution. It always clears inFlight and
// advances loadedAt (even on failure) so we never fetch more than once per TTL.
func (r *specRegistry) refresh(ctx context.Context) error {
	defer func() {
		r.mu.Lock()
		r.inFlight = false
		// Arm the TTL gate even on failure. This is intentionally unconditional:
		// a stale disk cache leaves loadedAt old, and without advancing it every
		// post-failure merge would spawn another background refresh until online.
		r.loadedAt = time.Now()
		r.mu.Unlock()
	}()

	fctx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		fctx, cancel = context.WithTimeout(ctx, r.timeout())
		defer cancel()
	}

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errStatus(resp.StatusCode)
	}
	body, err := readAllLimited(resp.Body, 16<<20) // 16 MiB cap
	if err != nil {
		return err
	}
	full, err := parseModelsDev(body)
	if err != nil {
		return err
	}
	if len(full) == 0 {
		return errEmptySpecs
	}

	now := time.Now()
	r.mu.Lock()
	r.indexLocked(full)
	r.loadedAt = now
	r.mu.Unlock()

	r.writeCache(specCacheFile{FetchedAt: now, Source: r.url, Specs: full})
	return nil
}

func (r *specRegistry) timeout() time.Duration {
	if r.client != nil && r.client.Timeout > 0 {
		return r.client.Timeout
	}
	return defaultSpecTimeout
}

// indexLocked populates byFull/byModel from a flat "provider/model" → spec map
// (the cache layout). Caller holds r.mu.
func (r *specRegistry) indexLocked(flat map[string]fetchedSpec) {
	full := make(map[string]fetchedSpec, len(flat))
	byModel := make(map[string]fetchedSpec, len(flat))
	for key, spec := range flat {
		lk := strings.ToLower(key)
		full[lk] = spec
		if idx := strings.LastIndex(lk, "/"); idx >= 0 && idx < len(lk)-1 {
			byModel[lk[idx+1:]] = spec
		} else {
			byModel[lk] = spec
		}
	}
	r.byFull = full
	r.byModel = byModel
}

// writeCache atomically persists the fetched table. Errors degrade silently.
func (r *specRegistry) writeCache(cf specCacheFile) {
	data, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(r.cachePath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	_ = store.WriteFileAtomic(r.cachePath, data, 0o644)
}

// parseModelsDev decodes the models.dev api.json into a flat
// "provider/model" → spec map. The bare "model" index is derived from this map
// by indexLocked, so the keying rule lives in one place.
func parseModelsDev(body []byte) (map[string]fetchedSpec, error) {
	var providers map[string]mdProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, err
	}
	full := map[string]fetchedSpec{}
	for providerID, prov := range providers {
		pl := strings.ToLower(strings.TrimSpace(providerID))
		for modelKey, m := range prov.Models {
			mk := strings.ToLower(strings.TrimSpace(modelKey))
			if mk == "" {
				continue
			}
			full[pl+"/"+mk] = fetchedSpec{
				ContextWindow:   m.Limit.Context,
				MaxOutputTokens: m.Limit.Output,
				InputCostPerM:   m.Cost.Input,
				OutputCostPerM:  m.Cost.Output,
				Reasoning:       m.Reasoning,
				ToolCall:        m.ToolCall,
				Temperature:     m.Temperature,
			}
		}
	}
	return full, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

var errEmptySpecs = errors.New("model specs: aggregator returned no models")

func errStatus(code int) error {
	return fmt.Errorf("model specs: unexpected status %d", code)
}

// readAllLimited reads up to max bytes, guarding against a runaway/huge body.
func readAllLimited(r io.Reader, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, max))
}
