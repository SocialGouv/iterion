package pricefeed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

func d(seconds int64) Duration { return Duration(time.Duration(seconds) * time.Second) }

// The one parser is strict: unknown fields, unpaired prices and negatives
// refuse; the refusal names the source the operator wrote.
func TestParseEntriesStrictness(t *testing.T) {
	if got, err := ParseEntries("", "OPENAI_COMPATIBLE_MODELS"); err != nil || len(got) != 0 {
		t.Fatalf("empty is the empty table, got %v err %v", got, err)
	}
	ok := `{"scaleway/glm-5.3":{"context_window":131072,"max_output_tokens":8192,"input_usd_per_mtok":0.6,"output_usd_per_mtok":1.8,"cache_read_usd_per_mtok":0.12}}`
	table, err := ParseEntries(ok, "feed stdout")
	if err != nil {
		t.Fatalf("a well-formed table refused: %v", err)
	}
	e := table["scaleway/glm-5.3"]
	if e.InputCostPerM != 0.6 || e.OutputCostPerM != 1.8 || e.CacheReadCostPerM != 0.12 || e.ContextWindow != 131072 {
		t.Fatalf("entry decoded wrong: %+v", e)
	}
	for _, bad := range []struct {
		raw  string
		want string
	}{
		{`{"m":{"input_usd_per_mtok":1}}`, "pair"},
		{`{"m":{"input_usd_per_mtok":-1,"output_usd_per_mtok":-1}}`, "negative"},
		{`{"m":{"inputt_usd_per_mtok":1}}`, "does not parse"},
	} {
		if _, err := ParseEntries(bad.raw, "feed stdout"); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("table %q: refusal %v does not say %q", bad.raw, err, bad.want)
		}
	}
	// The id normalization the operator table applies: the feed table
	// matches the catalog's spelling.
	upper, err := ParseEntries(`{"  ScaleWay/GLM-5.3  ":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := upper["scaleway/glm-5.3"]; !ok {
		t.Fatalf("id not normalized: %v", upper)
	}
}

func TestDurationWireIsSeconds(t *testing.T) {
	raw, err := json.Marshal(d(3600))
	if err != nil || string(raw) != "3600" {
		t.Fatalf("interval encodes as whole seconds, got %s err %v", raw, err)
	}
	var back Duration
	if err := json.Unmarshal([]byte("1800"), &back); err != nil || back != d(1800) {
		t.Fatalf("a JSON number of seconds did not decode: %v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`"90"`), &back); err != nil || back != d(90) {
		t.Fatalf("a string of seconds did not decode: %v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`"x"`), &back); err == nil {
		t.Fatal("garbage decoded as a duration")
	}
}

func freshEntry(feed string, refreshed time.Time, freshFor Duration, in, out float64) Entry {
	return Entry{Feed: feed, RefreshedAt: refreshed, FreshFor: freshFor,
		Spec: modelspecs.Spec{InputCostPerM: in, OutputCostPerM: out}}
}

func TestTableLookupOnlyAnswersFresh(t *testing.T) {
	now := time.Now()
	tbl := &Table{Entries: map[string]Entry{
		"fresh":    freshEntry("f", now.Add(-time.Minute), d(3600), 1, 2),
		"stale":    freshEntry("f", now.Add(-3*time.Hour), d(3600), 1, 2),
		"unwindow": freshEntry("f", now, 0, 1, 2),
	}}
	if e, ok := tbl.Lookup("fresh", now); !ok || e.Spec.InputCostPerM != 1 {
		t.Fatalf("a fresh entry did not answer: %+v %v", e, ok)
	}
	if _, ok := tbl.Lookup("stale", now); ok {
		t.Fatal("a stale entry answered — staleness must step down")
	}
	if _, ok := tbl.Lookup("unwindow", now); ok {
		t.Fatal("an entry with no freshness window answered")
	}
	if _, ok := tbl.Lookup("absent", now); ok {
		t.Fatal("an absent id answered")
	}
	if e, ok := tbl.Lookup("FRESH", now); !ok || e.Spec.InputCostPerM != 1 {
		t.Fatal("lookup is not case-normalized")
	}
}

func TestParseTableToleratesGarbage(t *testing.T) {
	if ParseTable("") != nil {
		t.Fatal("empty is no table")
	}
	if ParseTable("not json") != nil {
		t.Fatal("garbage became a table")
	}
	if ParseTable(`{"entries":null}`) != nil {
		t.Fatal("a null entries map became a table")
	}
	good := `{"entries":{"m":{"feed":"f","refreshed_at":"2026-11-07T10:00:00Z","fresh_for":3600,"spec":{"input_cost_per_m":1,"output_cost_per_m":2}}}}`
	if ParseTable(good) == nil {
		t.Fatal("the wire shape did not parse")
	}
}

// The fold's precedence, per ADR-163 §3: a fresh feed answers for the ids
// it carries; the more specific scope (team) answers over the less (org);
// within a scope the config's FIRST feed wins an id, deterministically;
// stale feeds contribute nothing.
func TestFoldPrecedence(t *testing.T) {
	now := time.Now()
	spec := func(in float64) modelspecs.Spec { return modelspecs.Spec{InputCostPerM: in, OutputCostPerM: in} }
	org := &ConfigRecord{Feeds: []FeedConfig{
		{Name: "org-first", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(30)},
		{Name: "org-second", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(30)},
	}}
	orgStates := map[string]*StateRecord{
		"org-first":  {Feed: "org-first", Entries: map[string]modelspecs.Spec{"a": spec(1), "c": spec(5)}, RefreshedAt: now, FreshFor: d(7200)},
		"org-second": {Feed: "org-second", Entries: map[string]modelspecs.Spec{"a": spec(2)}, RefreshedAt: now, FreshFor: d(7200)},
	}
	team := &ConfigRecord{Feeds: []FeedConfig{
		{Name: "team-feed", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(30)},
		{Name: "team-off", Type: TypeExec, Enabled: false, Interval: d(3600), Timeout: d(30)},
	}}
	teamStates := map[string]*StateRecord{
		"team-feed": {Feed: "team-feed", Entries: map[string]modelspecs.Spec{"a": spec(9), "b": spec(9)}, RefreshedAt: now, FreshFor: d(7200)},
		"team-off":  {Feed: "team-off", Entries: map[string]modelspecs.Spec{"z": spec(9)}, RefreshedAt: now, FreshFor: d(7200)},
	}
	table := Fold(FoldConfig{Now: now}, ScopeState{Config: org, States: orgStates}, ScopeState{Config: team, States: teamStates})
	if table == nil {
		t.Fatal("nothing folded")
	}
	if got := table.Entries["a"].Spec.InputCostPerM; got != 9 {
		t.Fatalf("the team feed must answer over the org for id a, got %v", got)
	}
	if got := table.Entries["b"].Spec.InputCostPerM; got != 9 {
		t.Fatalf("the team's own id did not fold: %v", got)
	}
	if got := table.Entries["c"].Spec.InputCostPerM; got != 5 {
		t.Fatalf("an org-only id did not fold: %v", got)
	}
	if got := table.Entries["a"].Feed; got != "team-feed" {
		t.Fatalf("provenance names the winning feed, got %q", got)
	}
	if _, ok := table.Entries["z"]; ok {
		t.Fatal("a disabled feed's entries folded")
	}
	// Within the org scope, the FIRST feed wins the contested id.
	orgOnly := Fold(FoldConfig{Now: now}, ScopeState{Config: org, States: orgStates})
	if got := orgOnly.Entries["a"].Spec.InputCostPerM; got != 1 {
		t.Fatalf("the config's first feed must win id a, got %v", got)
	}
	// A stale scope contributes nothing; the other scope still answers.
	staleOrg := map[string]*StateRecord{"org-first": {Feed: "org-first", Entries: map[string]modelspecs.Spec{"a": spec(1)}, RefreshedAt: now.Add(-24 * time.Hour), FreshFor: d(7200)}}
	table = Fold(FoldConfig{Now: now}, ScopeState{Config: org, States: staleOrg}, ScopeState{Config: team, States: teamStates})
	if got := table.Entries["a"].Spec.InputCostPerM; got != 9 {
		t.Fatalf("stale org, fresh team: team answers, got %v", got)
	}
	if Fold(FoldConfig{Now: now}, ScopeState{Config: org, States: staleOrg}) != nil {
		t.Fatal("a table of only stale entries folded to something")
	}
}

// The budget: a table over the channel's byte budget narrows to the run's
// named ids; one still over answers nil — the run keeps the operator floor
// rather than a channel the kernel would refuse to exec.
func TestFoldBudgetNarrowsThenRefuses(t *testing.T) {
	now := time.Now()
	entries := map[string]modelspecs.Spec{}
	for i := range 400 {
		entries[string(rune('a'+i%26))+string(rune('a'+i/26))+string(rune('a'+i%7))] =
			modelspecs.Spec{ContextWindow: 131072, MaxOutputTokens: 8192, InputCostPerM: 0.61, OutputCostPerM: 1.81}
	}
	state := &StateRecord{Feed: "big", Entries: entries, RefreshedAt: now, FreshFor: d(7200)}
	cfg := &ConfigRecord{Feeds: []FeedConfig{{Name: "big", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(30)}}}
	scope := ScopeState{Config: cfg, States: map[string]*StateRecord{"big": state}}

	var warned string
	if Fold(FoldConfig{Now: now, Budget: 2000, Warnf: func(f string, a ...any) { warned = f }}, scope) != nil {
		t.Fatal("an over-budget table with no named ids answered something")
	}
	if warned == "" || !strings.Contains(warned, "operator table") {
		t.Fatalf("an over-budget table warned wrongly: %q", warned)
	}
	warned = ""
	wide := Fold(FoldConfig{Now: now, Budget: 2000, Named: []string{"aaa", "Aba"}, Warnf: func(f string, a ...any) { warned = f }}, scope)
	if wide == nil {
		t.Fatal("a table narrowable to the named ids answered nil")
	}
	if warned == "" || !strings.Contains(warned, "narrowed") {
		t.Fatalf("the narrowing did not warn: %q", warned)
	}
	for id := range wide.Entries {
		if id != "aaa" && id != "aba" {
			t.Fatalf("the narrowed table carries %q, which the run did not name", id)
		}
	}
	warned = ""
	if got := Fold(FoldConfig{Now: now, Budget: 40, Named: []string{"aaa"}, Warnf: func(f string, a ...any) { warned = f }}, scope); got != nil {
		t.Fatalf("a table still over budget after narrowing answered %+v", got)
	}
	if warned == "" || !strings.Contains(warned, "operator table") {
		t.Fatalf("the refusal did not name the floor: %q", warned)
	}
}

func TestDeriveStatusVocabulary(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		entries  int
		age      time.Duration
		freshFor Duration
		err      string
		want     string
	}{
		{name: "fresh", entries: 1, age: time.Hour, freshFor: d(7200), want: StateOK},
		{name: "aged out", entries: 1, age: 3 * time.Hour, freshFor: d(7200), want: StateStale},
		{name: "failing with nothing", err: "boom", want: StateFailing},
		{name: "never ran", want: StateNever},
		{name: "failing with stale entries", entries: 1, age: 3 * time.Hour, freshFor: d(7200), err: "boom", want: StateStale},
	}
	for _, c := range cases {
		entries := map[string]modelspecs.Spec{}
		if c.entries > 0 {
			entries["m"] = modelspecs.Spec{InputCostPerM: 1, OutputCostPerM: 1}
		}
		refreshed := now.Add(-c.age)
		if c.entries == 0 {
			refreshed = time.Time{}
		}
		if got := DeriveStatus(entries, refreshed, c.freshFor, c.err, now); got != c.want {
			t.Errorf("%s: derived %q, want %q", c.name, got, c.want)
		}
	}
}

// The freshness window is twice the interval — the ADR's staleness bound,
// derived once so every reader agrees.
func TestFeedConfigFreshFor(t *testing.T) {
	f := FeedConfig{Interval: d(1800)}
	if f.FreshFor() != d(3600) {
		t.Fatalf("freshness window is not 2× the interval: %v", f.FreshFor())
	}
}

// Round 1's fixes, each witnessed: the strict parser refuses id
// collisions instead of flipping a coin, the fold never lets an unpriced
// entry shadow a priced floor, and a failed refresh keeps the window its
// entries were fetched under.
func TestParseEntriesRefusesCollidingSpellings(t *testing.T) {
	raw := `{"GLM-5.3":{"input_usd_per_mtok":1,"output_usd_per_mtok":2},"glm-5.3":{"input_usd_per_mtok":3,"output_usd_per_mtok":4}}`
	if _, err := ParseEntries(raw, "feed stdout"); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("a colliding spelling parsed: %v", err)
	}
	// Deterministic across many parses: the refusal is the only outcome.
	for range 50 {
		if _, err := ParseEntries(raw, "x"); err == nil {
			t.Fatal("a collision parsed on some iteration")
		}
	}
}

func TestFoldSkipsUnpricedEntries(t *testing.T) {
	now := time.Now()
	cfg := &ConfigRecord{Feeds: []FeedConfig{{Name: "f", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(30)}}}
	states := map[string]*StateRecord{"f": {Feed: "f",
		Entries: map[string]modelspecs.Spec{
			"priced":   {InputCostPerM: 1, OutputCostPerM: 2},
			"windowed": {ContextWindow: 131072},
			"half":     {InputCostPerM: 1},
		}, RefreshedAt: now, FreshFor: d(7200)}}
	var warned []string
	table := Fold(FoldConfig{Now: now, Warnf: func(f string, a ...any) { warned = append(warned, f) }}, ScopeState{Config: cfg, States: states})
	if _, ok := table.Entries["windowed"]; ok {
		t.Fatal("an entry without the price pair folded — it must never shadow the priced floor")
	}
	if _, ok := table.Entries["half"]; ok {
		t.Fatal("a half-priced entry folded")
	}
	if _, ok := table.Entries["priced"]; !ok {
		t.Fatal("a priced entry did not fold")
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "entries carry no price pair") {
		t.Fatalf("the skip must warn ONE summary line per fold: %v", warned)
	}
}

func TestRefreshFailureKeepsTheFetchedWindow(t *testing.T) {
	now := time.Now()
	feed := FeedConfig{Name: "f", Type: TypeExec, Enabled: true, Interval: d(3600), Timeout: d(10), Params: map[string]string{"command": "/nonexistent"}}
	// Entries fetched under a 2h window, three hours ago: stale.
	prev := &StateRecord{Feed: "f", Entries: map[string]modelspecs.Spec{"m": {InputCostPerM: 1, OutputCostPerM: 1}}, RefreshedAt: now.Add(-3 * time.Hour), FreshFor: d(7200)}
	// The config's interval has since been widened to 24h.
	feed.Interval = d(86400)
	state, outcome := Refresh(context.Background(), feed, prev, nil, now)
	if outcome.Status != StateFailing {
		t.Fatalf("the refresh did not fail: %+v", outcome)
	}
	if state.FreshFor != d(7200) {
		t.Fatalf("the failed refresh re-stamped the window to the current config: %v", state.FreshFor)
	}
	if DeriveStatus(state.Entries, state.RefreshedAt, state.FreshFor, state.LastError, now) != StateStale {
		t.Fatalf("the kept window did not derive stale: %+v", state)
	}
}

func TestDueTreatsAFutureAttemptAsDue(t *testing.T) {
	now := time.Now()
	feed := FeedConfig{Name: "f", Type: TypeExec, Interval: d(3600)}
	skewed := &StateRecord{LastAttempt: now.Add(30 * time.Minute)}
	if !Due(feed, skewed, now) {
		t.Fatal("a clock-skewed future LastAttempt suppressed the refresh")
	}
}

func TestValidateFeedRefusesUntrimmedName(t *testing.T) {
	feed := FeedConfig{Name: " scw", Type: TypeExec, Interval: d(60), Timeout: d(10)}
	if err := ValidateFeed(feed); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("an untrimmed name validated: %v", err)
	}
}

func TestDurationRefusesTruncatingStrings(t *testing.T) {
	for _, raw := range []string{`"90x"`, `"1.5"`, `"-5"`} {
		var d Duration
		if err := json.Unmarshal([]byte(raw), &d); err == nil {
			t.Errorf("%s parsed silently", raw)
		}
	}
}

// The memory twins hand out deep copies: mutating a returned record must
// not reach the store (round 1 caught the aliasing under -race).
func TestMemoryStoresDoNotAlias(t *testing.T) {
	ctx := context.Background()
	configs := NewMemoryConfigStore()
	if err := configs.Put(ctx, "doc", &ConfigRecord{Feeds: []FeedConfig{{Name: "f", Params: map[string]string{"token": "x"}}}}); err != nil {
		t.Fatal(err)
	}
	got, _ := configs.Get(ctx, "doc")
	got.Feeds[0].Params["token"] = "MUTATED"
	again, _ := configs.Get(ctx, "doc")
	if again.Feeds[0].Params["token"] != "x" {
		t.Fatal("a mutated returned record reached the store")
	}
	states := NewMemoryStateStore()
	if err := states.Put(ctx, "doc", &StateRecord{Entries: map[string]modelspecs.Spec{"m": {InputCostPerM: 1, OutputCostPerM: 1}}}); err != nil {
		t.Fatal(err)
	}
	sgot, _ := states.Get(ctx, "doc")
	sgot.Entries["m"] = modelspecs.Spec{InputCostPerM: 999, OutputCostPerM: 1}
	sagain, _ := states.Get(ctx, "doc")
	if sagain.Entries["m"].InputCostPerM != 1 {
		t.Fatal("a mutated state record reached the store")
	}
}

// One document, WHOLE: a stray closing bracket after a valid table
// refuses (dec.More() alone misses it — round 4), and whitespace-only
// tails stay clean.
func TestParseEntriesRefusesTrailingContent(t *testing.T) {
	good := `{"glm-5.3":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`
	if _, err := ParseEntries(good+"\n  ", "x"); err != nil {
		t.Fatalf("a whitespace tail refused: %v", err)
	}
	for _, bad := range []string{good + "}", good + "]", good + `{"m":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`, good + "x"} {
		if _, err := ParseEntries(bad, "x"); err == nil {
			t.Errorf("trailing content parsed silently: %.40s…", bad)
		}
	}
}

// The gate's finding, witnessed: an unchanged entry whose capability
// flags are populated does not report as changed — the *bool fields
// compare by VALUE, not by pointer address (every refresh allocates
// fresh pointers for the same verdicts).
func TestRefreshUnchangedFlaggedEntriesAreNotChanges(t *testing.T) {
	yes, no := true, false
	mk := func() map[string]modelspecs.Spec {
		return map[string]modelspecs.Spec{
			"glm-5.3": {ContextWindow: 131072, InputCostPerM: 0.6, OutputCostPerM: 1.8, Reasoning: &yes, ToolCall: &no, Temperature: &yes},
		}
	}
	prev := &StateRecord{Feed: "f", Entries: mk(), RefreshedAt: time.Now(), FreshFor: d(7200)}
	// An unchanged re-read (new pointers, same verdicts) reports nothing.
	state := StateRecord{Entries: mk()}
	changes := diffEntries(prev.Entries, state.Entries)
	if len(changes) != 0 {
		t.Fatalf("unchanged flagged entries reported as changed: %+v", changes)
	}
	// A REAL flag flip still reports.
	flippedEntry := mk()["glm-5.3"]
	flippedEntry.ToolCall = &yes
	changes = diffEntries(prev.Entries, map[string]modelspecs.Spec{"glm-5.3": flippedEntry})
	if len(changes) != 1 || changes[0].ID != "glm-5.3" {
		t.Fatalf("a real flag flip did not report: %+v", changes)
	}
}
