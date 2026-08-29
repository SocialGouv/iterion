// Package modelspecs is the dynamic model-spec registry: model metadata
// (context window, max output tokens, pricing, and the
// reasoning/tool_call/temperature flags) fetched from an online aggregator,
// cached on disk under ~/.iterion with a TTL, and served to callers as a
// source-agnostic Spec.
//
// It supplies; it does not decide. The curated static heuristics in
// pkg/backend/model remain AUTHORITATIVE whenever the aggregator lacks a model
// or the fetch fails/offline — brand-new models (e.g. glm-5.2) are not in
// aggregators yet, so the curated value must win. Merging the two is the
// caller's business, which is why this package holds no curated table.
//
// It is a LEAF package (its only iterion dependency is pkg/store, for the
// atomic cache write) so that pkg/backend/cost — itself a leaf, imported by
// model/ and delegate/ — can price a run from published rates without
// inverting that dependency.
//
// Resolution must NEVER block or slow a run: the synchronous path does only a
// cheap disk-cache read plus map lookups. The network fetch is strictly a
// background goroutine with a short timeout. The on-disk cache makes a freshly
// fetched table available to subsequent runs/processes.
package modelspecs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Source is models.dev, picked over LiteLLM's
// model_prices_and_context_window.json because it is provider-keyed (maps
// cleanly onto iterion's "provider/modelID" spec) and exposes all three
// capability flags directly (reasoning / tool_call / temperature) alongside
// limit.context, limit.output and cost.input/output. LiteLLM lacks a
// temperature flag and has a flatter, prefix-noisy key space; it remains a
// documented fallback source but is not wired here.
const Source = "https://models.dev/api.json"

const (
	defaultTTL     = 24 * time.Hour
	defaultTimeout = 3 * time.Second
)

// Spec is the normalized, source-agnostic shape extracted from the aggregator
// for a single model. The capability flags are pointers so that "omitted by the
// source" is distinguishable from an explicit false — an omitted flag lets the
// caller keep its curated heuristic.
//
// Every numeric field is zero-means-UNKNOWN, never zero-means-none: a price of
// 0 is "the aggregator had no price", not "free", and a MaxOutputTokens of 0 is
// "no published cap figure", not "uncapped". consensusSpec zeroes any field the
// publishers disagree on, so zero is a routine answer rather than an edge case.
type Spec struct {
	ContextWindow   int     `json:"context_window,omitempty"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	InputCostPerM   float64 `json:"input_cost_per_m,omitempty"`
	OutputCostPerM  float64 `json:"output_cost_per_m,omitempty"`
	Reasoning       *bool   `json:"reasoning,omitempty"`
	ToolCall        *bool   `json:"tool_call,omitempty"`
	Temperature     *bool   `json:"temperature,omitempty"`
}

// cacheFile is the on-disk cache layout. FetchedAt drives TTL freshness so the
// cache stays valid across copies (unlike file mtime).
type cacheFile struct {
	FetchedAt time.Time       `json:"fetched_at"`
	Source    string          `json:"source"`
	Specs     map[string]Spec `json:"specs"`
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
// Registry
// ---------------------------------------------------------------------------

// Options configures a Registry. Every field has a default, so New(Options{})
// is a valid environment-free registry; OptionsFromEnv fills them from the
// ITERION_MODEL_SPECS_* knobs.
type Options struct {
	// URL is the aggregator endpoint (ITERION_MODEL_SPECS_URL).
	URL string
	// CachePath is the on-disk cache location (ITERION_MODEL_SPECS_CACHE).
	CachePath string
	// TTL bounds cache freshness (ITERION_MODEL_SPECS_TTL).
	TTL time.Duration
	// Client performs the fetch; nil gets one with the default timeout.
	Client *http.Client
	// Disabled turns the registry into a no-op that never contributes and
	// never touches disk or network (ITERION_MODEL_SPECS=off).
	Disabled bool
	// NoAutoFetch suppresses the background refresh on a stale table. Tests
	// set it so an unrelated case never spawns a network goroutine.
	NoAutoFetch bool
	// ForceRefresh ignores initial cache freshness exactly once
	// (ITERION_MODEL_SPECS_REFRESH=1).
	ForceRefresh bool
}

// Registry serves aggregator specs, backed by an in-memory table refreshed from
// the disk cache and, in the background, from the network.
type Registry struct {
	mu        sync.Mutex
	url       string
	cachePath string
	ttl       time.Duration
	client    *http.Client
	enabled   bool
	autoFetch bool
	force     bool

	byFull    map[string]Spec // "provider/modelid" lowercased
	byModel   map[string]Spec // "modelid" lowercased
	loadedAt  time.Time       // last successful load/refresh or failed refresh attempt for TTL gating
	diskTried bool            // disk cache lazily loaded once
	inFlight  bool            // a background refresh is running

	// sealed marks a fixture-only registry (NewSeeded). Unlike enabled, it
	// still answers lookups — it just may never reach the network or the disk.
	sealed bool
}

// New builds a Registry from opts, filling unset fields with the package
// defaults.
func New(opts Options) *Registry {
	r := &Registry{
		url:       opts.URL,
		cachePath: opts.CachePath,
		ttl:       opts.TTL,
		client:    opts.Client,
		enabled:   !opts.Disabled,
		autoFetch: !opts.NoAutoFetch,
		force:     opts.ForceRefresh,
		byFull:    map[string]Spec{},
		byModel:   map[string]Spec{},
	}
	if r.url == "" {
		r.url = Source
	}
	if r.cachePath == "" {
		r.cachePath = DefaultCachePath()
	}
	if r.ttl <= 0 {
		r.ttl = defaultTTL
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: defaultTimeout}
	}
	return r
}

// NewSeeded builds a registry pre-loaded with a fixture table and wired to
// neither disk nor network. flat is keyed the way the aggregator's own table
// is — "provider/model", lowercased — so the bare index is derived by the very
// code the fetch path uses.
//
// It is exported as test support for the packages that CONSUME specs (the
// capability resolver, the cost estimator, the server): without it each would
// stand up an httptest server and drive a Refresh to assert on one price, and
// the cheaper alternative — letting them read the host's ~/.iterion cache —
// makes an assertion depend on whatever the developer's machine last fetched.
func NewSeeded(flat map[string]Spec) *Registry {
	r := New(Options{NoAutoFetch: true})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.indexLocked(flat)
	// Mark the table loaded and the disk read done: a seeded registry must
	// answer from its fixture alone, never fall back to the host's cache.
	r.diskTried = true
	r.loadedAt = time.Now()
	// NoAutoFetch only suppresses the BACKGROUND refresh. An explicit Refresh
	// would still reach models.dev and overwrite DefaultCachePath() — the
	// host's real ~/.iterion cache — which is precisely what this seam exists
	// to keep tests away from, and `iterion models --refresh` /
	// `models pricing --refresh` both route there through RefreshModelSpecs.
	// Sealing closes that door, so the promise above holds for every path and
	// not just the implicit one.
	r.sealed = true
	r.url = ""
	r.cachePath = ""
	return r
}

// OptionsFromEnv reads the ITERION_MODEL_SPECS_* knobs. It is called lazily by
// Default rather than at package init, so a process that sets the environment
// before first use (a test, a subprocess wrapper) is honoured.
func OptionsFromEnv() Options {
	// Reuse the package-wide env boolean grammar (0/false/off/no vs
	// 1/true/on/yes) so these knobs behave like ITERION_SECRETS_* etc.
	opts := Options{
		Disabled:     !envFlagEnabled("ITERION_MODEL_SPECS", true),
		ForceRefresh: envFlagEnabled("ITERION_MODEL_SPECS_REFRESH", false),
		URL:          strings.TrimSpace(os.Getenv("ITERION_MODEL_SPECS_URL")),
		CachePath:    strings.TrimSpace(os.Getenv("ITERION_MODEL_SPECS_CACHE")),
	}
	if v := strings.TrimSpace(os.Getenv("ITERION_MODEL_SPECS_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			opts.TTL = d
		}
	}
	return opts
}

// envFlagEnabled reads a boolean-ish env var. Recognised falsey values are "0",
// "false", "off", "no" (case-insensitive); anything else (including unset)
// returns def. Duplicated from pkg/backend/model rather than imported: this
// package must stay a leaf, and the grammar is ten lines.
func envFlagEnabled(name string, def bool) bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(name))) {
	case "":
		return def
	case "0", "false", "off", "no":
		return false
	case "1", "true", "on", "yes":
		return true
	default:
		return def
	}
}

// DefaultCachePath is where the fetched table is persisted between processes.
func DefaultCachePath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".iterion", "model-specs-cache.json")
	}
	// Fall back to a relative path; a failed write degrades silently anyway.
	return filepath.Join(".iterion", "model-specs-cache.json")
}

// ---------------------------------------------------------------------------
// process-wide default
// ---------------------------------------------------------------------------

var (
	defaultMu  sync.Mutex
	defaultReg *Registry
)

// Default returns the process-wide registry, building it from the environment
// on first use. Construction is LAZY on purpose: a package-var initializer
// would read the environment at import time, so a caller setting
// ITERION_MODEL_SPECS_CACHE in its own TestMain would be too late and would
// silently resolve against the host's real cache instead of its fixture.
func Default() *Registry {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultReg == nil {
		defaultReg = New(OptionsFromEnv())
	}
	return defaultReg
}

// SetDefault installs r as the process-wide registry and returns a function
// restoring the previous one. It is the cross-package test seam: resolution
// used to run off an unexported package var that in-package tests swapped
// directly, and extracting this package would otherwise have left callers (the
// cost estimator, the server) with no way to point resolution at a fixture
// instead of the host's ~/.iterion cache.
func SetDefault(r *Registry) (restore func()) {
	defaultMu.Lock()
	prev := defaultReg
	defaultReg = r
	defaultMu.Unlock()
	return func() {
		defaultMu.Lock()
		defaultReg = prev
		defaultMu.Unlock()
	}
}

// Lookup resolves the aggregator's spec for (provider, modelID), reporting
// whether the aggregator has one at all. A nil or disabled registry always
// reports false, which callers read as "keep your curated value".
//
// Resolution is by exact "provider/modelid" then by bare "modelid". The bare
// index is essential for GLM: it arrives as provider="anthropic",
// modelID="glm-5.2" (z.ai's Anthropic-compatible endpoint) but lives under a
// different provider in the aggregator.
func (r *Registry) Lookup(provider, modelID string) (Spec, bool) {
	if r == nil || !r.enabled {
		return Spec{}, false
	}
	r.ensureFresh()
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
	return Spec{}, false
}

// LookupBare resolves by bare model id alone, skipping the provider-qualified
// index. It serves callers that genuinely have no provider — a `.bot` may pin
// `model: "claude-opus-5"`, and the cost estimator is handed whatever string a
// backend reported — and must not invent one: the qualified index is the
// precise answer, and guessing a provider to reach it would report one
// publisher's numbers as if they were the model's.
func (r *Registry) LookupBare(modelID string) (Spec, bool) {
	if r == nil || !r.enabled {
		return Spec{}, false
	}
	r.ensureFresh()
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byModel[strings.ToLower(strings.TrimSpace(modelID))]
	return s, ok
}

// ensureFresh lazily loads the disk cache once, then triggers a background
// refresh when the in-memory table is stale (or absent). It never performs a
// synchronous network fetch — the caller is never blocked.
func (r *Registry) ensureFresh() {
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
			_ = r.Refresh(context.Background())
		}()
	}
}

// loadDiskCacheLocked reads the on-disk cache and, when fresh (or stale but
// present — better than nothing until a refresh lands), populates the in-memory
// indices. Caller holds r.mu. Any error degrades silently to no-op.
func (r *Registry) loadDiskCacheLocked() {
	data, err := os.ReadFile(r.cachePath)
	if err != nil {
		return
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil || len(cf.Specs) == 0 {
		return
	}
	r.indexLocked(cf.Specs)
	r.loadedAt = cf.FetchedAt
}

// Refresh performs the synchronous fetch+parse+cache+swap. Every failure path
// (DNS, timeout, non-2xx, malformed JSON, write error) is swallowed so a run is
// never blocked or failed by spec resolution. It always clears inFlight and
// advances loadedAt (even on failure) so we never fetch more than once per TTL.
//
// A nil, disabled or SEALED registry is a no-op returning nil: `iterion models
// --refresh` on a host with ITERION_MODEL_SPECS=off asked for nothing, and
// failing it would report an error about a feature the operator turned off. A
// sealed one (NewSeeded) is a test fixture whose whole point is that it answers
// from its own table without touching the host.
func (r *Registry) Refresh(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if !r.enabled || r.sealed {
		// Release any claim ensureFresh staked before spawning this call.
		// It cannot get here from that path today (a disabled registry never
		// reaches ensureFresh, a sealed one never auto-fetches), but the flag
		// is set by the CALLER and cleared by the defer below — so a no-op
		// return that skipped the defer would wedge inFlight for the life of
		// the process and silence the background refresh entirely.
		r.mu.Lock()
		r.inFlight = false
		r.mu.Unlock()
		return nil
	}
	defer func() {
		r.mu.Lock()
		r.inFlight = false
		// Arm the TTL gate even on failure. This is intentionally unconditional:
		// a stale disk cache leaves loadedAt old, and without advancing it every
		// post-failure lookup would spawn another background refresh until online.
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

	r.writeCache(cacheFile{FetchedAt: now, Source: r.url, Specs: full})
	return nil
}

func (r *Registry) timeout() time.Duration {
	if r.client != nil && r.client.Timeout > 0 {
		return r.client.Timeout
	}
	return defaultTimeout
}

// indexLocked populates byFull/byModel from a flat "provider/model" → spec map
// (the cache layout). Caller holds r.mu.
func (r *Registry) indexLocked(flat map[string]Spec) {
	full := make(map[string]Spec, len(flat))

	// The bare index used to be built by assigning into a map while ranging
	// over one — last writer wins, and Go randomises map iteration order. A
	// bare name published by several providers therefore resolved to a
	// DIFFERENT provider's numbers on every process start. Measured on a live
	// aggregator snapshot: 856 of 2740 bare names carry more than one
	// provider and 639 of those disagree. "glm-5.2" alone had 24 providers
	// quoting anywhere from 0 to 1.44 per million, so five consecutive runs
	// produced five different prices — and the same index feeds the context
	// window and the capability flags, where a silent 200K instead of 1M
	// truncates work rather than merely mis-reporting a cost.
	//
	// Grouping is now deterministic, and disagreement is resolved as UNKNOWN
	// rather than by picking a provider: a field survives only when every
	// candidate agrees on it. Zero/nil then falls through to the curated
	// value, which is authoritative anyway. Reporting nothing is recoverable;
	// reporting a confident number drawn at random is not.
	groups := make(map[string][]Spec, len(flat))
	keys := make([]string, 0, len(flat))
	for key := range flat {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lk := strings.ToLower(key)
		spec := flat[key]
		full[lk] = spec
		bare := lk
		if idx := strings.LastIndex(lk, "/"); idx >= 0 && idx < len(lk)-1 {
			bare = lk[idx+1:]
		}
		groups[bare] = append(groups[bare], spec)
	}

	byModel := make(map[string]Spec, len(groups))
	for bare, specs := range groups {
		byModel[bare] = consensusSpec(specs)
	}

	r.byFull = full
	r.byModel = byModel
}

// consensusSpec keeps only the fields every candidate agrees on. A single
// candidate is returned as-is; where candidates differ the field is zeroed
// (numbers) or nil'd (flags), which callers read as "the aggregator has no
// answer" and leave the curated value in place.
func consensusSpec(specs []Spec) Spec {
	if len(specs) == 0 {
		return Spec{}
	}
	out := specs[0]
	for _, s := range specs[1:] {
		if s.ContextWindow != out.ContextWindow {
			out.ContextWindow = 0
		}
		if s.MaxOutputTokens != out.MaxOutputTokens {
			out.MaxOutputTokens = 0
		}
		if s.InputCostPerM != out.InputCostPerM {
			out.InputCostPerM = 0
		}
		if s.OutputCostPerM != out.OutputCostPerM {
			out.OutputCostPerM = 0
		}
		out.Reasoning = agreedFlag(out.Reasoning, s.Reasoning)
		out.ToolCall = agreedFlag(out.ToolCall, s.ToolCall)
		out.Temperature = agreedFlag(out.Temperature, s.Temperature)
	}
	return out
}

// agreedFlag returns the shared value of two optional flags, or nil as soon as
// they differ or either side is unstated.
func agreedFlag(a, b *bool) *bool {
	if a == nil || b == nil || *a != *b {
		return nil
	}
	return a
}

// writeCache atomically persists the fetched table. Errors degrade silently.
func (r *Registry) writeCache(cf cacheFile) {
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
func parseModelsDev(body []byte) (map[string]Spec, error) {
	var providers map[string]mdProvider
	if err := json.Unmarshal(body, &providers); err != nil {
		return nil, err
	}
	full := map[string]Spec{}
	for providerID, prov := range providers {
		pl := strings.ToLower(strings.TrimSpace(providerID))
		for modelKey, m := range prov.Models {
			mk := strings.ToLower(strings.TrimSpace(modelKey))
			if mk == "" {
				continue
			}
			full[pl+"/"+mk] = Spec{
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
