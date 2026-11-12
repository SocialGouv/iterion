// Package pricefeed — the gateway price feed (ADR-163): operator pricing
// that syncs into the database and rides the existing IPC, while the
// operator table stays the floor.
//
// A feed is server-side, database-backed state at the org/team level of
// the provider/model override cascade. Runners never fetch prices: the
// server folds the effective table (fresh feed entries first, the
// operator floor below) and the answer travels with the run — sealed in
// the bundle, planted on the run's environment under one name the
// gateway catalog reads between the host IPC record and the operator
// table (compatgw.FeedTableEnv).
//
// The leaf discipline: this package imports modelspecs (the shared
// Spec), credroute (the env overlay the runner plants) and nothing else
// of iterion. The server, the publisher and the catalog all adapt onto
// it; a new source type is an adapter behind the Source seam, never an
// engine `if`.
package pricefeed

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// Source type names — the adapters behind the Source seam (ADR-163 §4).
// A new operator tariff shape is one adapter plus one RegisterSource
// line; the fold, the stores and the catalog never learn its name.
const (
	TypeModelsDev = "modelsdev"
	TypeScaleway  = "scaleway"
	TypeLiteLLM   = "litellm"
	TypeExec      = "exec"
)

// FeedConfig is one configured feed. It is tenant data: the operator's
// declaration of WHERE the tariffs come from and HOW OFTEN they sync.
type FeedConfig struct {
	Name     string            `json:"name" bson:"name"`
	Type     string            `json:"type" bson:"type"`
	Params   map[string]string `json:"params,omitempty" bson:"params,omitempty"`
	Enabled  bool              `json:"enabled" bson:"enabled"`
	Interval Duration          `json:"interval" bson:"interval"`
	Timeout  Duration          `json:"timeout" bson:"timeout"`
}

// FreshFor is the feed's freshness window: twice its interval. An entry
// older than that is STALE — a first-class state, not an error — and a
// stale feed steps down to the operator floor with a warning, never a
// refusal (ADR-163 §2/§3).
func (f FeedConfig) FreshFor() Duration { return 2 * f.Interval }

// ConfigRecord is one tenant's feed list — one document per scope
// (org or team), the twin of the routing-policy record.
type ConfigRecord struct {
	Feeds     []FeedConfig `json:"feeds" bson:"feeds"`
	UpdatedAt time.Time    `json:"updated_at" bson:"updated_at"`
	UpdatedBy string       `json:"updated_by,omitempty" bson:"updated_by,omitempty"`
}

// Feed state — the reconciler's durable answer, first-class (ADR-163 §2).
const (
	// StateOK: the last refresh succeeded and the entries are fresh.
	StateOK = "ok"
	// StateStale: entries exist but age past the freshness window — the
	// feed steps down; the operator table answers.
	StateStale = "stale"
	// StateFailing: the last attempt errored and nothing fresh remains.
	StateFailing = "failing"
	// StateNever: no refresh has succeeded yet.
	StateNever = "never"
)

// StateRecord is one feed's durable sync state, written by the elected
// reconciler and by the manual sync endpoint (both idempotent).
type StateRecord struct {
	Feed        string                     `json:"feed" bson:"feed"`
	Status      string                     `json:"status" bson:"status"`
	Entries     map[string]modelspecs.Spec `json:"entries,omitempty" bson:"entries,omitempty"`
	RefreshedAt time.Time                  `json:"refreshed_at,omitempty" bson:"refreshed_at,omitempty"`
	FreshFor    Duration                   `json:"fresh_for,omitempty" bson:"fresh_for,omitempty"`
	LastAttempt time.Time                  `json:"last_attempt,omitempty" bson:"last_attempt,omitempty"`
	LastError   string                     `json:"last_error,omitempty" bson:"last_error,omitempty"`
	UpdatedAt   time.Time                  `json:"updated_at" bson:"updated_at"`
}

// Fresh reports whether the record's entries are inside their freshness
// window at now. A record with no entries or no window is never fresh.
func (s *StateRecord) Fresh(now time.Time) bool {
	return s != nil && len(s.Entries) > 0 && time.Duration(s.FreshFor) > 0 &&
		!s.RefreshedAt.IsZero() && now.Sub(s.RefreshedAt) <= time.Duration(s.FreshFor)
}

// DeriveStatus folds (entries, ages, last error) into the first-class
// state vocabulary. Staleness is not an error: a feed whose entries aged
// out is stale whether its last attempt failed or simply nobody ran it.
func DeriveStatus(entries map[string]modelspecs.Spec, refreshedAt time.Time, freshFor Duration, lastErr string, now time.Time) string {
	fresh := len(entries) > 0 && time.Duration(freshFor) > 0 &&
		!refreshedAt.IsZero() && now.Sub(refreshedAt) <= time.Duration(freshFor)
	switch {
	case fresh:
		return StateOK
	case len(entries) > 0:
		return StateStale
	case strings.TrimSpace(lastErr) != "":
		return StateFailing
	default:
		return StateNever
	}
}

// Duration is a feed's interval/timeout on the wire: integer SECONDS.
// Operators write `"interval": 3600`, not nanoseconds — a unit a human
// cannot read is a unit a human mistypes.
type Duration time.Duration

// D converts a stdlib duration to the wire form.
func D(d time.Duration) Duration { return Duration(d) }

// MarshalJSON renders the duration as whole seconds.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(time.Duration(d) / time.Second))
}

// UnmarshalJSON accepts a whole number of seconds (JSON number or
// string); anything else refuses, naming the value.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var secs int64
	if err := json.Unmarshal(b, &secs); err == nil && secs >= 0 {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("pricefeed: duration must be whole seconds, got %s", string(b))
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || parsed < 0 {
		return fmt.Errorf("pricefeed: duration must be a whole number of seconds, got %q", raw)
	}
	*d = Duration(time.Duration(parsed) * time.Second)
	return nil
}

// Entry is one gateway id's folded answer: which feed carries it, how
// fresh that feed's word is, and the spec itself.
type Entry struct {
	Feed        string          `json:"feed"`
	RefreshedAt time.Time       `json:"refreshed_at"`
	FreshFor    Duration        `json:"fresh_for"`
	Spec        modelspecs.Spec `json:"spec"`
}

// Fresh reports whether the entry's feed word is inside its window at
// now. A reader past the window steps down to the operator floor — the
// entry stays visible in the table, it stops ANSWERING.
func (e Entry) Fresh(now time.Time) bool {
	return time.Duration(e.FreshFor) > 0 && !e.RefreshedAt.IsZero() &&
		now.Sub(e.RefreshedAt) <= time.Duration(e.FreshFor)
}

// Table is the wire shape: the effective table the server folds for one
// run, sealed in the bundle and planted on the run's environment. Every
// entry carries its own feed and freshness, so per-id provenance rides
// without a second channel.
type Table struct {
	Entries map[string]Entry `json:"entries"`
}

// Lookup answers one gateway id (already normalized: lower-cased,
// trimmed) when a FRESH feed entry carries it.
func (t *Table) Lookup(id string, now time.Time) (Entry, bool) {
	if t == nil {
		return Entry{}, false
	}
	e, ok := t.Entries[NormalizeID(id)]
	if !ok || !e.Fresh(now) {
		return Entry{}, false
	}
	return e, true
}

// ParseTable decodes the wire shape. Malformed or empty input answers
// nil: a garbage value degrades to the local sources, it never invents
// one (the contract the IPC record already holds).
func ParseTable(raw string) *Table {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var t Table
	if err := json.Unmarshal([]byte(raw), &t); err != nil || t.Entries == nil {
		return nil
	}
	return &t
}

// NormalizeID is the catalog's id spelling: lower-cased, trimmed — the
// same normalization the operator table's parser applies.
func NormalizeID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// ParseEntries is the strict parser of the operator table —
// OPENAI_COMPATIBLE_MODELS, `{"<gateway id>": {"context_window": N,
// "max_output_tokens": N, "input_usd_per_mtok": F, "output_usd_per_mtok": F,
// "cache_read_usd_per_mtok": F, "reasoning": B, "tool_call": B,
// "temperature": B}}`. Unknown fields are refused (a typo'd price silently
// ignored is worse than a refusal) — reasoning_efforts among them: accepted
// efforts are not this table's field (a feed's entries carry them, and the
// runner's env fallback has its own name, OPENAI_COMPATIBLE_REASONING_EFFORTS).
// Prices arrive as a pair or not
// at all, negatives are refused. compatgw's operator table parses through
// this function; a feed's entries parse through ParseFeedEntries, the same
// body with one more field. `what` names the source in the refusal — the env
// name, a feed's command — so the operator is told where to fix.
func ParseEntries(raw, what string) (map[string]modelspecs.Spec, error) {
	return parseEntries[operatorEntry](raw, what)
}

// ParseFeedEntries is the strict parser of a feed's entries — an exec
// feed's stdout and a declared feed's document: the operator-table format
// plus `"reasoning_efforts": [...]`, the model's accepted efforts in
// modelspecs.ParseEffortLevels' vocabulary. Every ParseEntries rule holds;
// on top, an effort list that is empty, null, repeats a level or names one
// outside the vocabulary refuses, naming the id.
func ParseFeedEntries(raw, what string) (map[string]modelspecs.Spec, error) {
	return parseEntries[feedEntry](raw, what)
}

// operatorEntry is the operator table's field set.
type operatorEntry struct {
	ContextWindow       int     `json:"context_window"`
	MaxOutputTokens     int     `json:"max_output_tokens"`
	InputUSDPerMTok     float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok    float64 `json:"output_usd_per_mtok"`
	CacheReadUSDPerMTok float64 `json:"cache_read_usd_per_mtok"`
	Reasoning           *bool   `json:"reasoning"`
	ToolCall            *bool   `json:"tool_call"`
	Temperature         *bool   `json:"temperature"`
}

func (e operatorEntry) spec(what, id string) (modelspecs.Spec, error) {
	if e.InputUSDPerMTok > 0 != (e.OutputUSDPerMTok > 0) {
		return modelspecs.Spec{}, fmt.Errorf("%s[%q]: the prices arrive as a pair or not at all", what, id)
	}
	if e.InputUSDPerMTok < 0 || e.OutputUSDPerMTok < 0 || e.CacheReadUSDPerMTok < 0 {
		return modelspecs.Spec{}, fmt.Errorf("%s[%q]: a price is negative", what, id)
	}
	return modelspecs.Spec{
		ContextWindow:     e.ContextWindow,
		MaxOutputTokens:   e.MaxOutputTokens,
		InputCostPerM:     e.InputUSDPerMTok,
		OutputCostPerM:    e.OutputUSDPerMTok,
		CacheReadCostPerM: e.CacheReadUSDPerMTok,
		Reasoning:         e.Reasoning,
		ToolCall:          e.ToolCall,
		Temperature:       e.Temperature,
	}, nil
}

// feedEntry is a feed's field set: the operator table's plus the accepted
// efforts. Absent reasoning_efforts leaves the spec's list nil (unstated).
type feedEntry struct {
	operatorEntry
	ReasoningEfforts json.RawMessage `json:"reasoning_efforts"`
}

func (e feedEntry) spec(what, id string) (modelspecs.Spec, error) {
	spec, err := e.operatorEntry.spec(what, id)
	if err != nil || e.ReasoningEfforts == nil {
		return spec, err
	}
	levels, err := modelspecs.ParseEffortLevels(e.ReasoningEfforts)
	if err != nil {
		return modelspecs.Spec{}, fmt.Errorf("%s[%q]: reasoning_efforts: %w", what, id, err)
	}
	if len(levels) == 0 {
		return modelspecs.Spec{}, fmt.Errorf("%s[%q]: reasoning_efforts is empty — list the accepted levels, or drop the field to state none", what, id)
	}
	seen := make(map[string]bool, len(levels))
	for _, level := range levels {
		if seen[level] {
			return modelspecs.Spec{}, fmt.Errorf("%s[%q]: reasoning_efforts repeats %q", what, id, level)
		}
		seen[level] = true
	}
	spec.ReasoningEfforts = levels
	return spec, nil
}

// parseEntries is the one body both parsers share: a strict decode of one
// whole document, then each entry through its field set's rules.
func parseEntries[E interface {
	spec(what, id string) (modelspecs.Spec, error)
}](raw, what string) (map[string]modelspecs.Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]modelspecs.Spec{}, nil
	}
	var entries map[string]E
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", what, err)
	}
	// One document, whole: trailing content after the first table (a
	// script printing two tables, a pasted half-edit, a stray closing
	// bracket — dec.More() alone misses those) refuses rather than
	// silently serving the first half.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%s carries content after the table", what)
	}
	table := map[string]modelspecs.Spec{}
	for id, e := range entries {
		spec, err := e.spec(what, id)
		if err != nil {
			return nil, err
		}
		// Two spellings that normalize to one key would hand the win to
		// map iteration order — the same table parsing to two different
		// price sets across processes. A strict parser refuses the coin
		// flip.
		if _, dup := table[NormalizeID(id)]; dup {
			return nil, fmt.Errorf("%s: id %q collides with another spelling of %q after normalization — keep one spelling", what, id, NormalizeID(id))
		}
		table[NormalizeID(id)] = spec
	}
	return table, nil
}

// FoldConfig bounds the fold: the serialized budget the table must fit
// (an environment variable is bounded by the kernel's per-string exec
// limit, and the crossing carries it beside everything else), and the
// run's NAMED gateway ids — the narrowing fallback when a table too wide
// for the channel would otherwise refuse to travel.
type FoldConfig struct {
	Now    time.Time
	Budget int
	Named  []string
	Warnf  func(format string, args ...any)
}

// DefaultBudget is the folded table's byte budget: 100 KiB, under the
// kernel's ~128 KiB per-env-string exec limit with room for the rest of
// a node's crossing environment.
const DefaultBudget = 100 * 1024

// ScopeState is one scope's contribution to the fold: its config
// document and its feeds' states. Scopes fold least-specific first —
// org, then team: a fresh team entry answers over a fresh org entry for
// the ids it carries, the direction the routing cascade already runs.
type ScopeState struct {
	Config *ConfigRecord
	States map[string]*StateRecord // feed name → state
}

// Fold composes the effective table for one run: every FRESH entry of
// every enabled feed, first feed in the config order winning within a
// scope, the later scope winning across them. Stale feeds contribute
// nothing — they step down to the operator floor, which is not this
// table's business.
//
// The result fits FoldConfig.Budget serialized. A table over budget is
// narrowed to the run's NAMED ids (the workflow's own gateway models);
// a narrowed table still over budget answers nil with a warning — the
// run keeps the operator floor rather than a channel the kernel would
// refuse to exec.
func Fold(cfg FoldConfig, scopes ...ScopeState) *Table {
	if cfg.Now.IsZero() {
		cfg.Now = time.Now()
	}
	if cfg.Budget <= 0 {
		cfg.Budget = DefaultBudget
	}
	entries := map[string]Entry{}
	for _, scope := range scopes {
		if scope.Config == nil {
			continue
		}
		// Each scope folds into its own answer first — the config's feed
		// order IS that scope's precedence, and a silent map-order
		// override would price an id at whichever feed ranged last —
		// then the scope replaces the ids it carries on the scopes below:
		// the team's feed answers over the org's, per id, the direction
		// the routing cascade already runs.
		scopeEntries := map[string]Entry{}
		for _, feed := range scope.Config.Feeds {
			if !feed.Enabled {
				continue
			}
			state := scope.States[feed.Name]
			if !state.Fresh(cfg.Now) {
				continue
			}
			unpriced := 0
			for id, spec := range state.Entries {
				id = NormalizeID(id)
				if id == "" {
					continue
				}
				// A price feed answers PRICES: an entry without the pair
				// (a source that carries the id but not its tariffs) must
				// never shadow the priced operator table — or the catalog
				// below it — with an unpriced answer (round 1's HIGH:
				// spend silently vanishing from metering and budgets).
				// It stays in the durable state, visible and honest; it
				// does not travel as the run's table. One SUMMARY line
				// per feed per fold, never per id — the fold runs on
				// every launch, and the ADR's watch is "not per
				// dispatch" (round 2).
				if spec.InputCostPerM <= 0 || spec.OutputCostPerM <= 0 {
					unpriced++
					continue
				}
				if _, taken := scopeEntries[id]; taken {
					continue
				}
				scopeEntries[id] = Entry{
					Feed:        feed.Name,
					RefreshedAt: state.RefreshedAt,
					FreshFor:    state.FreshFor,
					Spec:        spec,
				}
			}
			if unpriced > 0 && cfg.Warnf != nil {
				cfg.Warnf("price feed %s: %d entries carry no price pair — they do not answer and the operator table keeps those ids", feed.Name, unpriced)
			}
		}
		for id, e := range scopeEntries {
			entries[id] = e
		}
	}
	if len(entries) == 0 {
		return nil
	}
	table := &Table{Entries: entries}
	if table.Fits(cfg.Budget) {
		return table
	}
	narrowed := table.Narrow(cfg.Named)
	if narrowed != nil && narrowed.Fits(cfg.Budget) {
		if cfg.Warnf != nil {
			cfg.Warnf("price feed: the folded table exceeds %d bytes — narrowed to the run's %d named gateway ids", cfg.Budget, len(narrowed.Entries))
		}
		return narrowed
	}
	if cfg.Warnf != nil {
		cfg.Warnf("price feed: the folded table exceeds %d bytes even narrowed to the run's named ids — the run keeps the operator table (narrow the feed's source)", cfg.Budget)
	}
	return nil
}

// Fits reports whether the table serializes within budget.
func (t *Table) Fits(budget int) bool {
	if t == nil {
		return true
	}
	raw, err := json.Marshal(t)
	return err == nil && len(raw) <= budget
}

// Narrow keeps only the named ids (normalized). An empty answer is nil:
// narrowing to nothing is not a table.
func (t *Table) Narrow(named []string) *Table {
	if t == nil || len(named) == 0 {
		return nil
	}
	keep := map[string]Entry{}
	for _, id := range named {
		if e, ok := t.Entries[NormalizeID(id)]; ok {
			keep[NormalizeID(id)] = e
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return &Table{Entries: keep}
}

// IDs lists the table's ids, sorted — deterministic for tests and for
// the drift reporter.
func (t *Table) IDs() []string {
	if t == nil {
		return nil
	}
	ids := make([]string, 0, len(t.Entries))
	for id := range t.Entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
