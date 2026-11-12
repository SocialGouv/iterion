// Package pricefeed — the gateway price feed (ADR-163): operator pricing
// that syncs into the database and rides the existing IPC, while the
// operator table stays the floor.
//
// A feed is server-side, database-backed state at the platform, org and
// team levels of the provider/model override cascade. Runners never
// fetch prices: the server folds the effective table (answering feed
// entries first — a fetched feed's while fresh, a declaration's always —
// the operator floor below, and the models' accepted reasoning efforts in
// a map of their own) and the answer travels with the run — sealed in the
// bundle, planted on the run's environment under one name the gateway
// catalog reads between the host IPC record and the operator table
// (compatgw.FeedTableEnv).
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
	"maps"
	"slices"
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
	TypeDeclared  = "declared"
)

// FeedConfig is one configured feed — a tenant's or, at the platform
// level, the deployment's: the operator's declaration of WHERE the
// tariffs come from and HOW OFTEN they sync.
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

// ConfigRecord is one scope's feed list — one document per scope
// (platform, org or team), the twin of the routing-policy record.
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
	// StateDeclared: a static feed (a declaration). Its answer is its
	// config, read at every fold: it is never refreshed and never ages,
	// so no state document decides it — DeriveStatus never answers it,
	// the feed's view does.
	StateDeclared = "declared"
)

// StateRecord is one feed's durable sync state, written by the elected
// reconciler and by the manual sync endpoint (both idempotent) for a
// FETCHED feed only: a static feed (a declaration) writes none. Type
// stamps the source type the record was written for (Of).
type StateRecord struct {
	Feed        string                     `json:"feed" bson:"feed"`
	Type        string                     `json:"type,omitempty" bson:"type,omitempty"`
	Status      string                     `json:"status" bson:"status"`
	Entries     map[string]modelspecs.Spec `json:"entries,omitempty" bson:"entries,omitempty"`
	RefreshedAt time.Time                  `json:"refreshed_at,omitempty" bson:"refreshed_at,omitempty"`
	FreshFor    Duration                   `json:"fresh_for,omitempty" bson:"fresh_for,omitempty"`
	LastAttempt time.Time                  `json:"last_attempt,omitempty" bson:"last_attempt,omitempty"`
	LastError   string                     `json:"last_error,omitempty" bson:"last_error,omitempty"`
	UpdatedAt   time.Time                  `json:"updated_at" bson:"updated_at"`
}

// Of answers the record when it was written for a feed of source type
// typ, nil otherwise. A state document is keyed by (scope, feed name)
// alone, so a feed retyped under the same name finds its predecessor's
// document there; that document is not the feed's word — not its answer,
// not the entries a failing refresh retains, not its status. A record
// with no stamp predates the stamp and counts as the feed's own.
func (s *StateRecord) Of(typ string) *StateRecord {
	if s == nil || (s.Type != "" && s.Type != typ) {
		return nil
	}
	return s
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
// fresh that feed's word is, and the spec itself. A declared entry (a
// static feed's) carries the config's UpdatedAt as RefreshedAt and no
// window: a declaration does not age. Its Spec never carries accepted
// efforts — those ride in Table.Efforts.
type Entry struct {
	Feed        string          `json:"feed"`
	RefreshedAt time.Time       `json:"refreshed_at"`
	FreshFor    Duration        `json:"fresh_for"`
	Spec        modelspecs.Spec `json:"spec"`
	Declared    bool            `json:"declared,omitempty"`
}

// EffortsEntry is one gateway id's accepted reasoning efforts on the run
// table: the feed that stated them and the wire values, in the order the
// feed listed them.
type EffortsEntry struct {
	Feed   string   `json:"feed"`
	Values []string `json:"values"`
}

// Fresh reports whether the entry's feed word answers at now: a declared
// entry always does; a fetched one while inside its window. A reader past
// the window steps down to the operator floor — the entry stays visible
// in the table, it stops ANSWERING.
func (e Entry) Fresh(now time.Time) bool {
	if e.Declared {
		return true
	}
	return time.Duration(e.FreshFor) > 0 && !e.RefreshedAt.IsZero() &&
		now.Sub(e.RefreshedAt) <= time.Duration(e.FreshFor)
}

// Table is the wire shape: the effective table the server folds for one
// run, sealed in the bundle and planted on the run's environment. Every
// entry carries its own feed and freshness, so per-id provenance rides
// without a second channel.
//
// Two maps, folded independently: Entries answers prices, windows and
// flags; Efforts answers accepted reasoning efforts. An id may sit in
// either or both — a feed that prices an id and a declaration that only
// lists its efforts each answer their own field. Entries is always
// encoded, `{}` when empty: an older runner's parser requires it, so an
// efforts-only table still plants (and its efforts map is ignored there).
type Table struct {
	Entries map[string]Entry        `json:"entries"`
	Efforts map[string]EffortsEntry `json:"efforts,omitempty"`
}

// newTable builds a table whose price map is never nil, so it encodes as
// `{}`; an empty efforts map is omitted.
func newTable(entries map[string]Entry, efforts map[string]EffortsEntry) *Table {
	if entries == nil {
		entries = map[string]Entry{}
	}
	if len(efforts) == 0 {
		efforts = nil
	}
	return &Table{Entries: entries, Efforts: efforts}
}

// Lookup answers one gateway id (normalized here: lower-cased, trimmed)
// when an entry that answers at now (Entry.Fresh) carries it.
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

// ParseTable decodes the wire shape: a table carrying prices, efforts or
// both. Malformed or empty input — or neither map — answers nil: a
// garbage value degrades to the local sources, it never invents one (the
// contract the IPC record already holds). The answer's Entries is never
// nil.
func ParseTable(raw string) *Table {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var t Table
	if err := json.Unmarshal([]byte(raw), &t); err != nil || (t.Entries == nil && t.Efforts == nil) {
		return nil
	}
	if t.Entries == nil {
		t.Entries = map[string]Entry{}
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
// Prices arrive as a pair or not at all, negatives are refused. compatgw's
// operator table parses through this function; a feed's entries parse
// through ParseFeedEntries, the same body with one more field. `what` names
// the source in the refusal — the env name, a feed's command — so the
// operator is told where to fix.
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
// document and its fetched feeds' states. Scopes fold least-specific
// first — platform, then org, then team: a more specific scope's entry
// answers over a less specific one's for the ids it carries, the
// direction the routing cascade already runs. A static feed's entries
// are read from Config itself (its params), never from States.
type ScopeState struct {
	Config *ConfigRecord
	States map[string]*StateRecord // feed name → state
}

// Fold composes the effective table for one run, two fields folded
// independently, each by the same precedence — within a scope the
// config's FIRST enabled feed stating the field for an id wins it; across
// scopes the later (more specific) scope replaces the ids it carries:
//
//   - Entries: every priced entry (input and output price both above
//     zero) of every answering feed, its accepted efforts stripped;
//   - Efforts: every entry's accepted efforts, priced or not.
//
// A fetched feed answers while its state is fresh (StateRecord.Fresh);
// a stale one contributes nothing — it steps down to the operator floor,
// which is not this table's business. A static feed (a declaration)
// answers from its config at every fold, whatever its age and whatever
// state document sits under its name; a declaration the write rules
// refuse steps down with a warning naming the feed.
//
// An entry without the price pair never shadows the priced operator
// table — or the catalog below it — with an unpriced answer: it stays in
// the durable state, visible, and does not travel in Entries. One summary
// warning per feed per fold counts the unpriced entries whose window,
// output cap, cache price or flags are thereby dropped (or that state
// nothing); an entry whose only content is its efforts is not counted,
// its efforts answer.
//
// The result is nil only when both maps are empty, and fits
// FoldConfig.Budget serialized. A table over budget narrows its price
// map to the run's NAMED ids (the workflow's own gateway models) and
// keeps every id's accepted efforts: a dropped price steps down to the
// operator floor, a dropped effort list would send a level the gateway
// refuses — and a run whose model is an env dial names nothing. When
// the efforts alone exceed the budget, both maps narrow to the named
// ids; a table still over budget answers nil with a warning — the run
// keeps the operator floor rather than a channel the kernel would
// refuse to exec.
func Fold(cfg FoldConfig, scopes ...ScopeState) *Table {
	if cfg.Now.IsZero() {
		cfg.Now = time.Now()
	}
	if cfg.Budget <= 0 {
		cfg.Budget = DefaultBudget
	}
	entries := map[string]Entry{}
	efforts := map[string]EffortsEntry{}
	for _, scope := range scopes {
		if scope.Config == nil {
			continue
		}
		// Each scope folds into its own answer first — the config's feed
		// order IS that scope's precedence, and a silent map-order
		// override would answer an id from whichever feed ranged last —
		// then the scope replaces the ids it carries on the scopes below.
		scopeEntries := map[string]Entry{}
		scopeEfforts := map[string]EffortsEntry{}
		for _, feed := range scope.Config.Feeds {
			if !feed.Enabled {
				continue
			}
			answer, ok := feedAnswer(cfg, scope, feed)
			if !ok {
				continue
			}
			unpriced := 0
			for id, spec := range answer.specs {
				id = NormalizeID(id)
				if id == "" {
					continue
				}
				if len(spec.ReasoningEfforts) > 0 {
					if _, taken := scopeEfforts[id]; !taken {
						scopeEfforts[id] = EffortsEntry{Feed: feed.Name, Values: slices.Clone(spec.ReasoningEfforts)}
					}
				}
				if spec.InputCostPerM <= 0 || spec.OutputCostPerM <= 0 {
					if len(spec.ReasoningEfforts) == 0 || len(unpricedFields(spec)) > 0 {
						unpriced++
					}
					continue
				}
				if _, taken := scopeEntries[id]; taken {
					continue
				}
				spec.ReasoningEfforts = nil
				scopeEntries[id] = Entry{
					Feed:        feed.Name,
					RefreshedAt: answer.refreshedAt,
					FreshFor:    answer.freshFor,
					Spec:        spec,
					Declared:    answer.declared,
				}
			}
			if unpriced > 0 && cfg.Warnf != nil {
				cfg.Warnf("price feed %s: %d entries carry no price pair — their windows, prices and flags do not answer and the operator table keeps those ids", feed.Name, unpriced)
			}
		}
		maps.Copy(entries, scopeEntries)
		maps.Copy(efforts, scopeEfforts)
	}
	if len(entries) == 0 && len(efforts) == 0 {
		return nil
	}
	table := newTable(entries, efforts)
	if table.Fits(cfg.Budget) {
		return table
	}
	// Prices narrow first: a price the narrowing drops steps down to the
	// operator floor, an effort list it dropped would send a level the
	// gateway refuses — and efforts come from declarations and exec
	// feeds, a few bytes per id.
	if pricesNarrowed := newTable(narrowEntries(entries, cfg.Named), efforts); (len(pricesNarrowed.Entries) > 0 || len(pricesNarrowed.Efforts) > 0) && pricesNarrowed.Fits(cfg.Budget) {
		if cfg.Warnf != nil {
			cfg.Warnf("price feed: the folded table exceeds %d bytes — prices narrowed to the run's %d named gateway ids, accepted efforts kept for all %d ids", cfg.Budget, len(pricesNarrowed.Entries), len(pricesNarrowed.Efforts))
		}
		return pricesNarrowed
	}
	narrowed := table.Narrow(cfg.Named)
	if narrowed != nil && narrowed.Fits(cfg.Budget) {
		if cfg.Warnf != nil {
			cfg.Warnf("price feed: the folded table exceeds %d bytes — narrowed to the run's %d named gateway ids", cfg.Budget, len(narrowed.IDs()))
		}
		return narrowed
	}
	if cfg.Warnf != nil {
		cfg.Warnf("price feed: the folded table exceeds %d bytes even narrowed to the run's named ids — the run keeps the operator table (narrow the feed's source)", cfg.Budget)
	}
	return nil
}

// feedAnswered is one feed's contribution to a fold: its entries and the
// provenance every entry it answers carries.
type feedAnswered struct {
	specs       map[string]modelspecs.Spec
	refreshedAt time.Time
	freshFor    Duration
	declared    bool
}

// feedAnswer reads what one enabled feed answers at the fold's instant:
// a static feed its config's params (declared, stamped with the config's
// UpdatedAt, no window), a fetched feed its own state (StateRecord.Of)
// while fresh.
func feedAnswer(cfg FoldConfig, scope ScopeState, feed FeedConfig) (feedAnswered, bool) {
	if src, ok := SourceFor(feed.Type); ok {
		if static, ok := src.(StaticSource); ok {
			specs, err := static.Entries(feed.Params)
			if err != nil {
				if cfg.Warnf != nil {
					cfg.Warnf("price feed %s: the declaration does not answer — %v", feed.Name, err)
				}
				return feedAnswered{}, false
			}
			return feedAnswered{specs: specs, refreshedAt: scope.Config.UpdatedAt, declared: true}, true
		}
	}
	state := scope.States[feed.Name].Of(feed.Type)
	if !state.Fresh(cfg.Now) {
		return feedAnswered{}, false
	}
	return feedAnswered{specs: state.Entries, refreshedAt: state.RefreshedAt, freshFor: state.FreshFor}, true
}

// Fits reports whether the table serializes within budget — both maps.
func (t *Table) Fits(budget int) bool {
	if t == nil {
		return true
	}
	raw, err := json.Marshal(t)
	return err == nil && len(raw) <= budget
}

// narrowEntries keeps the named ids (normalized) of a price map.
func narrowEntries(entries map[string]Entry, named []string) map[string]Entry {
	keep := map[string]Entry{}
	for _, id := range named {
		id = NormalizeID(id)
		if e, ok := entries[id]; ok {
			keep[id] = e
		}
	}
	return keep
}

// Narrow keeps only the named ids (normalized), in both maps. An answer
// carrying neither is nil: narrowing to nothing is not a table.
func (t *Table) Narrow(named []string) *Table {
	if t == nil || len(named) == 0 {
		return nil
	}
	keep := map[string]Entry{}
	keepEfforts := map[string]EffortsEntry{}
	for _, id := range named {
		id = NormalizeID(id)
		if e, ok := t.Entries[id]; ok {
			keep[id] = e
		}
		if e, ok := t.Efforts[id]; ok {
			keepEfforts[id] = e
		}
	}
	if len(keep) == 0 && len(keepEfforts) == 0 {
		return nil
	}
	return newTable(keep, keepEfforts)
}

// IDs lists the ids either map carries, each once, sorted —
// deterministic for tests and for the fold's narrowing warning.
func (t *Table) IDs() []string {
	if t == nil {
		return nil
	}
	ids := make([]string, 0, len(t.Entries)+len(t.Efforts))
	for id := range t.Entries {
		ids = append(ids, id)
	}
	for id := range t.Efforts {
		if _, priced := t.Entries[id]; !priced {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
