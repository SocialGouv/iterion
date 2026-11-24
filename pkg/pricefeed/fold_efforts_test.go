package pricefeed

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func declaredFeed(name, entries string) FeedConfig {
	return FeedConfig{Name: name, Type: TypeDeclared, Enabled: true, Interval: d(30 * 86400), Timeout: d(1),
		Params: map[string]string{"entries": entries}}
}

func fetchedFeed(name string) FeedConfig {
	return FeedConfig{Name: name, Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(30)}
}

func freshState(feed string, now time.Time, entries map[string]modelspecs.Spec) *StateRecord {
	return &StateRecord{Feed: feed, Entries: entries, RefreshedAt: now, FreshFor: d(7200)}
}

// Accepted efforts fold per FIELD, independently of the price: a fetched
// feed that prices an id and a declaration that only lists its efforts
// both answer, each for its own field, whichever comes first in the
// config. Within a scope the first feed stating efforts for an id wins
// them, as the first priced feed wins the price.
func TestFoldEffortsFoldPerField(t *testing.T) {
	now := time.Now()
	priced := fetchedFeed("litellm")
	efforts := declaredFeed("pliage", `{"Scaleway/DeepSeek-X":{"reasoning_efforts":["low","high","xhigh","max"]}}`)
	states := map[string]*StateRecord{"litellm": freshState("litellm", now, map[string]modelspecs.Spec{
		"scaleway/deepseek-x": {InputCostPerM: 0.2, OutputCostPerM: 0.8},
	})}
	for _, order := range [][]FeedConfig{{priced, efforts}, {efforts, priced}} {
		names := order[0].Name + "," + order[1].Name
		table := Fold(FoldConfig{Now: now}, ScopeState{Config: &ConfigRecord{Feeds: order, UpdatedAt: now}, States: states})
		if table == nil {
			t.Fatalf("[%s] nothing folded", names)
		}
		if got := table.Entries["scaleway/deepseek-x"]; got.Feed != "litellm" || got.Spec.InputCostPerM != 0.2 {
			t.Fatalf("[%s] the price answers from %q (%+v), want litellm", names, got.Feed, got.Spec)
		}
		want := EffortsEntry{Feed: "pliage", Values: []string{"low", "high", "xhigh", "max"}}
		if got := table.Efforts["scaleway/deepseek-x"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("[%s] efforts folded %+v, want %+v", names, got, want)
		}
	}

	// Two feeds both stating efforts: the config's first wins them, and
	// the efforts never ride inside the price entry.
	both := fetchedFeed("exec-gw")
	bothStates := map[string]*StateRecord{"exec-gw": freshState("exec-gw", now, map[string]modelspecs.Spec{
		"scaleway/deepseek-x": {InputCostPerM: 0.2, OutputCostPerM: 0.8, ReasoningEfforts: []string{"low"}},
	})}
	for _, c := range []struct {
		order []FeedConfig
		want  string
	}{
		{[]FeedConfig{both, efforts}, "exec-gw"},
		{[]FeedConfig{efforts, both}, "pliage"},
	} {
		table := Fold(FoldConfig{Now: now}, ScopeState{Config: &ConfigRecord{Feeds: c.order, UpdatedAt: now}, States: bothStates})
		if got := table.Efforts["scaleway/deepseek-x"].Feed; got != c.want {
			t.Fatalf("feed order %s,%s: efforts answered from %q, want the first stating feed %q", c.order[0].Name, c.order[1].Name, got, c.want)
		}
		if got := table.Entries["scaleway/deepseek-x"].Spec.ReasoningEfforts; got != nil {
			t.Fatalf("the price entry carries efforts %v — the run reads them from the efforts map only", got)
		}
	}
}

// Across scopes the more specific one replaces the efforts per id, as it
// does the price: a team's list beats the platform's, and a platform
// list reaches a team whose own feed only prices the id.
func TestFoldEffortsScopePrecedence(t *testing.T) {
	now := time.Now()
	platform := ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("pliage", `{"deepseek-x":{"reasoning_efforts":["low","high"]},"glm-5.3":{"reasoning_efforts":["max"]}}`),
	}}}
	team := ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("team-decl", `{"deepseek-x":{"reasoning_efforts":["xhigh"]}}`),
		fetchedFeed("team-prices"),
	}}, States: map[string]*StateRecord{"team-prices": freshState("team-prices", now, map[string]modelspecs.Spec{
		"glm-5.3": {InputCostPerM: 2.01, OutputCostPerM: 6.15},
	})}}
	table := Fold(FoldConfig{Now: now}, platform, team)
	if got := table.Efforts["deepseek-x"]; got.Feed != "team-decl" || !reflect.DeepEqual(got.Values, []string{"xhigh"}) {
		t.Fatalf("the team's efforts must answer over the platform's, got %+v", got)
	}
	if got := table.Efforts["glm-5.3"]; got.Feed != "pliage" || !reflect.DeepEqual(got.Values, []string{"max"}) {
		t.Fatalf("the platform's efforts must reach a team that only prices the id, got %+v", got)
	}
	if got := table.Entries["glm-5.3"]; got.Feed != "team-prices" {
		t.Fatalf("the team's price must answer, got %+v", got)
	}
	// The scope order IS the precedence: reversed, the platform answers.
	if got := Fold(FoldConfig{Now: now}, team, platform).Efforts["deepseek-x"]; got.Feed != "pliage" {
		t.Fatalf("scopes folded least-specific-last must let the last answer, got %+v", got)
	}
}

// A declaration does not age: its answer is its config, read at every
// fold. A year on, the declared price and efforts still answer, while a
// fetched feed refreshed at the same instant has long stepped down. A
// state document under the declared feed's name is not its answer.
func TestFoldDeclarationNeverAges(t *testing.T) {
	declaredAt := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	later := declaredAt.Add(365 * 24 * time.Hour)
	cfg := &ConfigRecord{UpdatedAt: declaredAt, Feeds: []FeedConfig{
		declaredFeed("pliage", `{"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},"deepseek-x":{"reasoning_efforts":["low","high"]}}`),
		fetchedFeed("scw"),
	}}
	states := map[string]*StateRecord{
		"scw": freshState("scw", declaredAt, map[string]modelspecs.Spec{"gpt-oss": {InputCostPerM: 1, OutputCostPerM: 1}}),
		// A state the fold must not read for a static feed: fresh at
		// `later`, with other numbers.
		"pliage": freshState("pliage", later, map[string]modelspecs.Spec{"glm-5.3": {InputCostPerM: 99, OutputCostPerM: 99}}),
	}
	table := Fold(FoldConfig{Now: later}, ScopeState{Config: cfg, States: states})
	if table == nil {
		t.Fatal("a declaration aged out of the fold")
	}
	got, ok := table.Entries["glm-5.3"]
	if !ok || got.Spec.InputCostPerM != 2.01 || got.Spec.ContextWindow != 256000 {
		t.Fatalf("the declared entry answered %+v (present %v), want the config's 2.01 and 256000", got, ok)
	}
	if !got.Declared || !got.RefreshedAt.Equal(declaredAt) || got.FreshFor != 0 {
		t.Fatalf("the declared entry's provenance is %+v, want declared, refreshed_at = the config's UpdatedAt, no window", got)
	}
	if e, ok := table.Lookup("glm-5.3", later.Add(10*365*24*time.Hour)); !ok || e.Feed != "pliage" {
		t.Fatalf("Lookup judged a declaration by a window it does not have: %+v %v", e, ok)
	}
	if table.Efforts["deepseek-x"].Feed != "pliage" {
		t.Fatalf("declared efforts aged out: %+v", table.Efforts)
	}
	if _, ok := table.Entries["gpt-oss"]; ok {
		t.Fatal("a fetched feed of the same age answered past its window")
	}
	// No state at all: the declaration still answers.
	if table := Fold(FoldConfig{Now: later}, ScopeState{Config: cfg}); table == nil || table.Entries["glm-5.3"].Spec.InputCostPerM != 2.01 {
		t.Fatalf("a declaration with no state document did not answer: %+v", table)
	}
	// Disabled, it says nothing.
	off := *cfg
	off.Feeds = []FeedConfig{cfg.Feeds[0]}
	off.Feeds[0].Enabled = false
	if table := Fold(FoldConfig{Now: later}, ScopeState{Config: &off}); table != nil {
		t.Fatalf("a disabled declaration folded: %+v", table)
	}
}

// A stored declaration the write rules refuse steps down with a warning
// naming the feed; the scope's other feeds still answer.
func TestFoldStepsDownABrokenDeclaration(t *testing.T) {
	now := time.Now()
	cfg := &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("broken", `{"glm-5.3":{}}`),
		fetchedFeed("scw"),
	}}
	states := map[string]*StateRecord{"scw": freshState("scw", now, map[string]modelspecs.Spec{"glm-5.3": {InputCostPerM: 1, OutputCostPerM: 2}})}
	var warned []string
	table := Fold(FoldConfig{Now: now, Warnf: func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }}, ScopeState{Config: cfg, States: states})
	if table == nil || table.Entries["glm-5.3"].Feed != "scw" {
		t.Fatalf("the scope's fetched feed did not answer beside a broken declaration: %+v", table)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "broken") || !strings.Contains(warned[0], "states nothing") {
		t.Fatalf("the broken declaration must warn once, naming the feed and the rule: %v", warned)
	}
}

// An efforts-only fold is a table: the run receives its efforts, the
// price map is present and empty (an older runner's parser requires it),
// and the unpriced summary does not count entries whose efforts answer.
func TestFoldEffortsOnlyTable(t *testing.T) {
	now := time.Now()
	cfg := &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("pliage", `{"deepseek-x":{"reasoning_efforts":["high"]}}`),
		fetchedFeed("mixed"),
	}}
	states := map[string]*StateRecord{"mixed": freshState("mixed", now, map[string]modelspecs.Spec{
		"other":      {ReasoningEfforts: []string{"low"}},
		"windowed":   {ContextWindow: 1000, ReasoningEfforts: []string{"low"}},
		"bare-empty": {},
	})}
	var warned []string
	table := Fold(FoldConfig{Now: now, Warnf: func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }}, ScopeState{Config: cfg, States: states})
	if table == nil {
		t.Fatal("an efforts-only fold answered nil — the run would lose its efforts")
	}
	if table.Entries == nil || len(table.Entries) != 0 {
		t.Fatalf("an efforts-only table's price map is %#v, want present and empty", table.Entries)
	}
	if len(table.Efforts) != 3 {
		t.Fatalf("efforts folded %+v, want deepseek-x, other and windowed", table.Efforts)
	}
	raw, err := json.Marshal(table)
	if err != nil || !strings.Contains(string(raw), `"entries":{}`) {
		t.Fatalf("an efforts-only table encodes as %s (%v), want an explicit empty entries map", raw, err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "mixed: 2 entries carry no price pair") {
		t.Fatalf("the unpriced summary must count the windowed and the empty entry of feed mixed, not the efforts-only ones: %v", warned)
	}
}

// The wire, as an older runner reads it: a frozen copy of the
// pre-efforts Table and ParseTable decodes the new bytes — priced entries
// answer, the efforts map is ignored, and an efforts-only table still
// parses (its entries are an empty map, not null).
func TestOldRunnerReadsTheNewTable(t *testing.T) {
	type frozenSpec struct {
		ContextWindow     int     `json:"context_window,omitempty"`
		MaxOutputTokens   int     `json:"max_output_tokens,omitempty"`
		InputCostPerM     float64 `json:"input_cost_per_m,omitempty"`
		OutputCostPerM    float64 `json:"output_cost_per_m,omitempty"`
		CacheReadCostPerM float64 `json:"cache_read_cost_per_m,omitempty"`
		Reasoning         *bool   `json:"reasoning,omitempty"`
		ToolCall          *bool   `json:"tool_call,omitempty"`
		Temperature       *bool   `json:"temperature,omitempty"`
	}
	type frozenEntry struct {
		Feed        string     `json:"feed"`
		RefreshedAt time.Time  `json:"refreshed_at"`
		FreshFor    Duration   `json:"fresh_for"`
		Spec        frozenSpec `json:"spec"`
	}
	type frozenTable struct {
		Entries map[string]frozenEntry `json:"entries"`
	}
	frozenParse := func(raw string) *frozenTable {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil
		}
		var t frozenTable
		if err := json.Unmarshal([]byte(raw), &t); err != nil || t.Entries == nil {
			return nil
		}
		return &t
	}

	now := time.Now()
	mixed := Fold(FoldConfig{Now: now}, ScopeState{
		Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
			declaredFeed("pliage", `{"deepseek-x":{"reasoning_efforts":["low","high"]},"glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15,"reasoning_efforts":["max"]}}`),
		}},
	})
	raw, err := json.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}
	old := frozenParse(string(raw))
	if old == nil {
		t.Fatalf("an older runner refuses the new table: %s", raw)
	}
	if e := old.Entries["glm-5.3"]; e.Feed != "pliage" || e.Spec.InputCostPerM != 2.01 || e.Spec.OutputCostPerM != 6.15 {
		t.Fatalf("an older runner reads the priced entry as %+v", e)
	}
	if _, ok := old.Entries["deepseek-x"]; ok {
		t.Fatal("an efforts-only id reached the price map an older runner answers from")
	}

	effortsOnly := Fold(FoldConfig{Now: now}, ScopeState{
		Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("pliage", `{"deepseek-x":{"reasoning_efforts":["high"]}}`)}},
	})
	raw, err = json.Marshal(effortsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if old := frozenParse(string(raw)); old == nil || len(old.Entries) != 0 {
		t.Fatalf("an older runner does not plant an efforts-only table %s: %+v", raw, old)
	}
	// The narrowed form keeps the explicit empty price map too.
	narrowed := effortsOnly.Narrow([]string{"DeepSeek-X"})
	raw, err = json.Marshal(narrowed)
	if err != nil {
		t.Fatal(err)
	}
	if old := frozenParse(string(raw)); old == nil {
		t.Fatalf("an older runner refuses a narrowed efforts-only table %s", raw)
	}
}

// The current parser plants a table that carries prices, efforts, or
// both, and still refuses garbage.
func TestParseTableAcceptsEntriesOrEfforts(t *testing.T) {
	got := ParseTable(`{"efforts":{"deepseek-x":{"feed":"pliage","values":["low","high"]}}}`)
	if got == nil {
		t.Fatal("an efforts-only table without an entries key did not parse")
	}
	if want := (EffortsEntry{Feed: "pliage", Values: []string{"low", "high"}}); !reflect.DeepEqual(got.Efforts["deepseek-x"], want) {
		t.Fatalf("efforts parsed %+v, want %+v", got.Efforts["deepseek-x"], want)
	}
	if got.Entries == nil {
		t.Fatal("a parsed table's price map is nil")
	}
	for _, garbage := range []string{`{"entries":null}`, `{"efforts":null}`, `{}`, `not json`} {
		if ParseTable(garbage) != nil {
			t.Errorf("%s became a table", garbage)
		}
	}
}

// Narrow keeps the run's named ids in BOTH maps; narrowing to nothing in
// either is not a table.
func TestNarrowKeepsBothMaps(t *testing.T) {
	table := &Table{
		Entries: map[string]Entry{"glm-5.3": {Feed: "a", Spec: modelspecs.Spec{InputCostPerM: 1, OutputCostPerM: 2}}, "other": {Feed: "a"}},
		Efforts: map[string]EffortsEntry{"deepseek-x": {Feed: "b", Values: []string{"high"}}, "glm-5.3": {Feed: "b", Values: []string{"max"}}, "far": {Feed: "b", Values: []string{"low"}}},
	}
	got := table.Narrow([]string{"GLM-5.3", "deepseek-x"})
	if got == nil {
		t.Fatal("narrowing to named ids answered nil")
	}
	if len(got.Entries) != 1 || got.Entries["glm-5.3"].Feed != "a" {
		t.Fatalf("narrowed prices %+v", got.Entries)
	}
	if len(got.Efforts) != 2 || got.Efforts["deepseek-x"].Feed != "b" || got.Efforts["glm-5.3"].Feed != "b" {
		t.Fatalf("narrowed efforts %+v, want deepseek-x and glm-5.3", got.Efforts)
	}
	if ids := got.IDs(); !reflect.DeepEqual(ids, []string{"deepseek-x", "glm-5.3"}) {
		t.Fatalf("the narrowed table's ids are %v, want each id of either map once", ids)
	}
	effortsOnly := table.Narrow([]string{"far"})
	if effortsOnly == nil || effortsOnly.Entries == nil || len(effortsOnly.Entries) != 0 || effortsOnly.Efforts["far"].Feed != "b" {
		t.Fatalf("narrowing to an efforts-only id answered %+v", effortsOnly)
	}
	if table.Narrow([]string{"absent"}) != nil {
		t.Fatal("narrowing to no carried id answered a table")
	}
}

// The byte budget measures the WHOLE table, efforts included: an
// efforts map too wide for the channel narrows to the run's named ids,
// and one still too wide answers nil. The ids are random so nothing
// about the payload compresses or repeats.
func TestFoldBudgetCountsEfforts(t *testing.T) {
	now := time.Now()
	var doc strings.Builder
	doc.WriteString("{")
	var named string
	for i := range 300 {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		id := "gw/" + hex.EncodeToString(buf)
		if i == 0 {
			named = id
		} else {
			doc.WriteString(",")
		}
		doc.WriteString(`"` + id + `":{"reasoning_efforts":["low","medium","high","xhigh","max"]}`)
	}
	doc.WriteString("}")
	scope := ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("pliage", doc.String())}}}

	whole := Fold(FoldConfig{Now: now}, scope)
	if whole == nil || len(whole.Efforts) != 300 {
		t.Fatalf("the unbounded fold lost efforts: %v", whole)
	}
	raw, _ := json.Marshal(whole)
	budget := len(raw) / 4

	var warned string
	narrowed := Fold(FoldConfig{Now: now, Budget: budget, Named: []string{named}, Warnf: func(f string, a ...any) { warned = f }}, scope)
	if narrowed == nil || len(narrowed.Efforts) != 1 {
		t.Fatalf("an efforts map over budget did not narrow to the named id: %d efforts", effortsLen(narrowed))
	}
	if !strings.Contains(warned, "narrowed") {
		t.Fatalf("the narrowing did not warn: %q", warned)
	}
	if got := Fold(FoldConfig{Now: now, Budget: budget}, scope); got != nil {
		t.Fatalf("an efforts map over budget with no named id answered %d efforts", effortsLen(got))
	}
}

func effortsLen(t *Table) int {
	if t == nil {
		return -1
	}
	return len(t.Efforts)
}

// A table over budget narrows a fetched feed's PRICES to the run's named
// ids and keeps the accepted efforts of every id: a price the narrowing
// drops steps
// down to the operator floor, an effort list it dropped would send a
// level the gateway refuses. A run whose gateway model comes from an env
// dial (`${VAR:-…}`) names nothing, and still carries the platform's
// declared efforts. The priced ids are random, so the budget is crossed
// by bytes that do not compress or repeat.
func TestFoldOverBudgetKeepsDeclaredEfforts(t *testing.T) {
	const wire = "scaleway/deepseek-v4.1-flash"
	now := time.Now()
	platform := ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("pliage", `{"`+wire+`":{"reasoning_efforts":["low","high","xhigh","max"]}}`),
	}}}
	priced := map[string]modelspecs.Spec{}
	var another string
	for i := range 600 {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		id := "openrouter/" + hex.EncodeToString(buf)
		if i == 0 {
			another = id
		}
		priced[id] = modelspecs.Spec{ContextWindow: 131072, MaxOutputTokens: 16384, InputCostPerM: 0.3, OutputCostPerM: 1.2, CacheReadCostPerM: 0.03}
	}
	team := ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{fetchedFeed("models-dev")}},
		States: map[string]*StateRecord{"models-dev": freshState("models-dev", now, priced)}}
	if whole := Fold(FoldConfig{Now: now, Budget: 1 << 30}, platform, team); whole.Fits(DefaultBudget) {
		t.Fatal("the whole table fits the default budget — the case does not cross it")
	}
	for _, tc := range []struct {
		name  string
		named []string
	}{
		{"an env dial names nothing", nil},
		{"the run names another id", []string{another}},
		{"the run names the id", []string{wire}},
	} {
		var warned []string
		got := Fold(FoldConfig{Now: now, Named: tc.named, Warnf: func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }}, platform, team)
		if got == nil {
			t.Fatalf("[%s] the over-budget fold answered nil — the platform's declared efforts for %s are gone (warnings %v)", tc.name, wire, warned)
		}
		if !got.Fits(DefaultBudget) {
			t.Fatalf("[%s] the answer does not fit the budget", tc.name)
		}
		if e := got.Efforts[wire]; e.Feed != "pliage" || len(e.Values) != 4 {
			t.Fatalf("[%s] efforts[%s] = %+v, want the platform declaration's four levels", tc.name, wire, got.Efforts)
		}
		for id := range got.Entries {
			if !slices.Contains(tc.named, id) {
				t.Fatalf("[%s] the narrowed price map carries %s, which the run did not name", tc.name, id)
			}
		}
		if slices.Contains(tc.named, another) {
			if _, ok := got.Entries[another]; !ok {
				t.Fatalf("[%s] the named priced id %s was dropped", tc.name, another)
			}
		}
		if len(warned) != 1 || !strings.Contains(warned[0], fmt.Sprintf("prices narrowed to %d ids — %d the run names, 0 with accepted efforts, 0 declared", len(got.Entries), len(got.Entries))) ||
			!strings.Contains(warned[0], "accepted efforts kept for all 1 ids") {
			t.Fatalf("[%s] the narrowing must warn once, counting prices and efforts apart: %v", tc.name, warned)
		}
	}
}

// A price entry answers WHOLE from one feed (ADR-122): the fold never
// merges a window, an output cap or a cache price across feeds or
// levels. When the winning entry leaves unstated a field the entry it
// shadows states, the fold says so — one line per (winner, shadowed) pair
// naming the ids and the fields — and keeps the winner whole. The
// efforts stay the declaration's either way.
func TestFoldWarnsWhenAWinningEntryLeavesShadowedFieldsUnstated(t *testing.T) {
	const wire = "scaleway/deepseek-v4.1-flash"
	now := time.Now()
	declared := declaredFeed("pliage", `{"`+wire+`":{"context_window":256000,"max_output_tokens":32768,"input_usd_per_mtok":0.672,"output_usd_per_mtok":2.016,"cache_read_usd_per_mtok":0.1344,"reasoning_efforts":["low","high","xhigh","max"]}}`)
	litellm := fetchedFeed("pliage-litellm")
	pricesOnly := map[string]*StateRecord{"pliage-litellm": freshState("pliage-litellm", now, map[string]modelspecs.Spec{wire: {InputCostPerM: 0.7, OutputCostPerM: 2.1}})}
	restated := map[string]*StateRecord{"pliage-litellm": freshState("pliage-litellm", now, map[string]modelspecs.Spec{wire: {ContextWindow: 256000, MaxOutputTokens: 32768, InputCostPerM: 0.7, OutputCostPerM: 2.1, CacheReadCostPerM: 0.14}})}
	scope := func(states map[string]*StateRecord, feeds ...FeedConfig) ScopeState {
		return ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: feeds}, States: states}
	}
	dropped := []string{"context_window", "max_output_tokens", "cache_read_usd_per_mtok"}
	for _, tc := range []struct {
		name    string
		scopes  []ScopeState
		winner  string
		window  int
		dropped []string
	}{
		{"a team's litellm over the platform's declaration", []ScopeState{scope(nil, declared), scope(pricesOnly, litellm)}, "pliage-litellm", 0, dropped},
		{"an org's litellm over the platform's declaration", []ScopeState{scope(nil, declared), scope(pricesOnly, litellm), scope(nil, declaredFeed("team-decl", `{"other":{"input_usd_per_mtok":1,"output_usd_per_mtok":1}}`))}, "pliage-litellm", 0, dropped},
		{"one level, the litellm feed listed first", []ScopeState{scope(pricesOnly, litellm, declared)}, "pliage-litellm", 0, dropped},
		{"one level, the declaration listed first", []ScopeState{scope(pricesOnly, declared, litellm)}, "pliage", 256000, nil},
		{"a team's litellm restating the fields", []ScopeState{scope(nil, declared), scope(restated, litellm)}, "pliage-litellm", 256000, nil},
	} {
		var warned []string
		table := Fold(FoldConfig{Now: now, Warnf: func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }}, tc.scopes...)
		if table == nil {
			t.Fatalf("[%s] nothing folded", tc.name)
		}
		entry := table.Entries[wire]
		if entry.Feed != tc.winner || entry.Spec.ContextWindow != tc.window {
			t.Fatalf("[%s] the entry answers from %q with window %d, want %q whole (window %d)", tc.name, entry.Feed, entry.Spec.ContextWindow, tc.winner, tc.window)
		}
		if tc.winner == "pliage-litellm" && tc.dropped != nil && (entry.Spec.MaxOutputTokens != 0 || entry.Spec.CacheReadCostPerM != 0) {
			t.Fatalf("[%s] the winning entry was merged with the shadowed one: %+v", tc.name, entry.Spec)
		}
		if got := table.Efforts[wire].Feed; got != "pliage" {
			t.Fatalf("[%s] the efforts answer from %q, want the declaration", tc.name, got)
		}
		if tc.dropped == nil {
			if len(warned) != 0 {
				t.Fatalf("[%s] the fold warned with nothing left unstated: %v", tc.name, warned)
			}
			continue
		}
		if len(warned) != 1 {
			t.Fatalf("[%s] want one shadowing warning, got %v", tc.name, warned)
		}
		for _, want := range append([]string{"price feed pliage-litellm answers whole over feed pliage and leaves", wire}, tc.dropped...) {
			if !strings.Contains(warned[0], want) {
				t.Fatalf("[%s] the warning %q does not name %s", tc.name, warned[0], want)
			}
		}
	}
}

// A declared entry answers WHOLE, as the operator-table row it copies
// did: a pool's row stating a window and an output cap without prices
// declares as written and travels in Entries, unpriced, with no warning —
// while a fetched feed's same entry still never travels (ADR-163: a
// source listing an id it does not price says nothing about it). A
// declared row without the price pair never hides a price: listed first
// in its level beside a feed that prices the id, it steps aside, the
// priced entry answers whole, and the fold says which fields of the row
// do not answer.
func TestFoldADeclaredUnpricedRowAnswersWhole(t *testing.T) {
	const id = "scaleway/qwen-unpriced"
	now := time.Now()
	row := modelspecs.Spec{ContextWindow: 131072, MaxOutputTokens: 16384}
	declared := declaredFeed("pliage", `{"`+id+`":{"context_window":131072,"max_output_tokens":16384}}`)
	var warned []string
	warnf := func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }

	table := Fold(FoldConfig{Now: now, Warnf: warnf}, ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declared}}})
	if table == nil {
		t.Fatal("a declared unpriced row folded to nothing")
	}
	if e := table.Entries[id]; e.Feed != "pliage" || !e.Declared || !SpecEqual(e.Spec, row) {
		t.Fatalf("the declared row answers %+v, want pliage's window and output cap, unpriced", e)
	}
	if len(warned) != 0 {
		t.Fatalf("a declared row warned: %v", warned)
	}

	fetched := fetchedFeed("scw")
	states := map[string]*StateRecord{"scw": freshState("scw", now, map[string]modelspecs.Spec{id: row})}
	if got := Fold(FoldConfig{Now: now, Warnf: warnf}, ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{fetched}}, States: states}); got != nil {
		t.Fatalf("a fetched unpriced entry travelled: %+v", got)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "scw: 1 entries carry no price pair") {
		t.Fatalf("the fetched unpriced entry must be counted: %v", warned)
	}

	warned = nil
	priced := map[string]*StateRecord{"scw": freshState("scw", now, map[string]modelspecs.Spec{id: {InputCostPerM: 0.3, OutputCostPerM: 1.2}})}
	table = Fold(FoldConfig{Now: now, Warnf: warnf}, ScopeState{Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declared, fetched}}, States: priced})
	if e := table.Entries[id]; e.Feed != "scw" || e.Declared || e.Spec.InputCostPerM != 0.3 {
		t.Fatalf("beside a feed pricing the id, the declared row listed first answers %+v, want scw's priced entry whole", e)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "price feed pliage states context_window, max_output_tokens without the price pair for 1 ids ("+id+")") ||
		!strings.Contains(warned[0], "the priced entry of feed scw answers them whole") {
		t.Fatalf("the row stepping aside for a price must say so: %v", warned)
	}
}

// A declared row without the price pair never hides a price below it
// either: a more specific level's row — a flag beside its efforts, a
// window alone, a pool's window and output cap — leaves the less specific
// level's priced entry answering whole, warned naming both feeds with
// their levels and the row's fields, while the row's efforts answer. Two
// unpriced rows at two levels keep the more specific one, and a priced
// row of a more specific level replaces an unpriced one below it.
func TestFoldADeclaredUnpricedRowNeverHidesAPriceBelowIt(t *testing.T) {
	const id = "scaleway/deepseek-v4.1-flash"
	now := time.Now()
	platform := ScopeState{Label: "platform", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("pliage", `{"`+id+`":{"context_window":256000,"max_output_tokens":32768,"input_usd_per_mtok":0.672,"output_usd_per_mtok":2.016}}`),
	}}}
	for _, tc := range []struct {
		name, row, fields string
		efforts           bool
	}{
		{"a flag beside its efforts", `{"reasoning":true,"reasoning_efforts":["low","high","xhigh","max"]}`, "reasoning", true},
		{"a window alone", `{"context_window":128000}`, "context_window", false},
		{"a pool's window and output cap", `{"context_window":128000,"max_output_tokens":8192}`, "context_window, max_output_tokens", false},
	} {
		var warned []string
		warnf := func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }
		team := ScopeState{Label: "team t1", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("mine", `{"`+id+`":`+tc.row+`}`)}}}
		table := Fold(FoldConfig{Now: now, Warnf: warnf}, platform, team)
		if e := table.Entries[id]; e.Feed != "pliage" || e.Spec.InputCostPerM != 0.672 || e.Spec.ContextWindow != 256000 {
			t.Errorf("[%s] the team's unpriced row hides the platform's price: %+v", tc.name, e)
		}
		if got := table.Efforts[id]; tc.efforts != (got.Feed == "mine") {
			t.Errorf("[%s] efforts answer %+v, want the team's: %v", tc.name, got, tc.efforts)
		}
		want := "price feed mine (team t1) states " + tc.fields + " without the price pair for 1 ids (" + id + ")"
		if len(warned) != 1 || !strings.Contains(warned[0], want) || !strings.Contains(warned[0], "the priced entry of feed pliage (platform) answers them whole") {
			t.Errorf("[%s] warnings %q, want one naming %q and pliage (platform)", tc.name, warned, want)
		}
	}

	unpricedPlatform := ScopeState{Label: "platform", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("pliage", `{"`+id+`":{"context_window":256000}}`)}}}
	unpricedTeam := ScopeState{Label: "team t1", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("mine", `{"`+id+`":{"context_window":128000}}`)}}}
	if e := Fold(FoldConfig{Now: now}, unpricedPlatform, unpricedTeam).Entries[id]; e.Feed != "mine" || e.Spec.ContextWindow != 128000 {
		t.Errorf("two unpriced rows: %+v, want the team's", e)
	}
	pricedTeam := ScopeState{Label: "team t1", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("mine", `{"`+id+`":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`)}}}
	if e := Fold(FoldConfig{Now: now}, unpricedPlatform, pricedTeam).Entries[id]; e.Feed != "mine" || e.Spec.InputCostPerM != 1 {
		t.Errorf("a priced team row over an unpriced platform row: %+v, want the team's", e)
	}
	// A tenant's price is its runs' price, however low: the rule bars an
	// unpriced row, not a cheap one.
	cheapTeam := ScopeState{Label: "team t1", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("mine", `{"`+id+`":{"input_usd_per_mtok":1e-12,"output_usd_per_mtok":1e-12}}`)}}}
	if e := Fold(FoldConfig{Now: now}, platform, cheapTeam).Entries[id]; e.Feed != "mine" || e.Spec.InputCostPerM != 1e-12 {
		t.Errorf("a near-zero team price over the platform's: %+v, want the team's", e)
	}
}

// FoldProvenance names, per id, the scope each field of the table answers
// from — per field, as the fold decides it: an entry the platform prices
// and efforts the team states; an unpriced declared row of the team that
// steps aside for the platform's price; the more specific of two like
// answers. Its table is Fold's.
func TestFoldProvenanceNamesTheScopeOfEachField(t *testing.T) {
	now := time.Now()
	platform := ScopeState{Label: "platform", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("pliage", `{"deepseek-x":{"input_usd_per_mtok":0.6,"output_usd_per_mtok":2.4,"reasoning_efforts":["low","high"]},"glm-5.3":{"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},"kimi-k2":{"input_usd_per_mtok":1,"output_usd_per_mtok":3,"reasoning_efforts":["max"]}}`),
	}}}
	team := ScopeState{Label: "team t1", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{
		declaredFeed("pliage", `{"deepseek-x":{"reasoning_efforts":["xhigh"]},"glm-5.3":{"context_window":256000},"kimi-k2":{"input_usd_per_mtok":1,"output_usd_per_mtok":3,"reasoning_efforts":["max"]}}`),
	}}}
	table, provenance := FoldProvenance(FoldConfig{Now: now}, platform, team)
	if want := Fold(FoldConfig{Now: now}, platform, team); !reflect.DeepEqual(table, want) {
		t.Fatalf("FoldProvenance's table %+v is not Fold's %+v", table, want)
	}
	for _, c := range []struct {
		id, entry, efforts string
	}{
		{"deepseek-x", "platform", "team t1"},
		{"glm-5.3", "platform", ""},
		{"kimi-k2", "team t1", "team t1"},
	} {
		if got := provenance.Entries[c.id]; got != c.entry {
			t.Errorf("%s: the entry answers from %q, want %q", c.id, got, c.entry)
		}
		if got := provenance.Efforts[c.id]; got != c.efforts {
			t.Errorf("%s: the efforts answer from %q, want %q", c.id, got, c.efforts)
		}
	}
	if got := table.Entries["glm-5.3"].Spec.InputCostPerM; got != 2.01 {
		t.Errorf("glm-5.3's entry prices %v, want the platform's 2.01 (the team's unpriced row steps aside)", got)
	}
}
