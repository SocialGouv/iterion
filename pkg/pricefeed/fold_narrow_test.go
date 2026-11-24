package pricefeed

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// randomIDs answers n gateway ids under prefix, random so nothing about a
// table built from them compresses or repeats.
func randomIDs(t *testing.T, prefix string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		ids[i] = prefix + hex.EncodeToString(buf)
	}
	return ids
}

// pricedIDs is a fetched feed's entries: every id priced with a window.
func pricedIDs(ids []string) map[string]modelspecs.Spec {
	out := map[string]modelspecs.Spec{}
	for _, id := range ids {
		out[id] = modelspecs.Spec{ContextWindow: 131072, MaxOutputTokens: 16384, InputCostPerM: 0.3, OutputCostPerM: 1.2, CacheReadCostPerM: 0.03}
	}
	return out
}

// declaredDoc is a declaration's document: every id priced with a window.
func declaredDoc(ids []string) string {
	var b strings.Builder
	b.WriteString("{")
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s":{"input_usd_per_mtok":1,"output_usd_per_mtok":2,"context_window":128000}`, id)
	}
	b.WriteString("}")
	return b.String()
}

// retireDeclaration is the platform declaration a retired pool leaves:
// deepseek with its accepted efforts, glm-5.3 priced without any.
const retireDeclaration = `{"scaleway/deepseek-v4.1-flash":{"context_window":256000,"max_output_tokens":32768,"input_usd_per_mtok":0.672,"output_usd_per_mtok":2.016,"reasoning_efforts":["low","high","xhigh","max"]},"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`

// A table over budget keeps, beyond the prices of the ids a run names, the
// prices of the ids with accepted efforts and of the declared ids: a run
// whose gateway model is an env dial (`${VAR:-…}`) names no id, and a
// retired pool's rows — carried into a declaration — price it as the
// pool's env table did, whatever a fetched feed adds. The narrowing gives
// up the declared ids' prices, then the efforts ids', only as the budget
// needs, and every id's accepted efforts stay.
func TestFoldOverBudgetKeepsThePricesOfTheIDsWithEffortsAndTheDeclaredIDs(t *testing.T) {
	const deepseek, glm = "scaleway/deepseek-v4.1-flash", "glm-5.3"
	now := time.Now()
	platform := ScopeState{Label: "platform", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("pliage", retireDeclaration)}}}
	fetchedIDs := randomIDs(t, "openrouter/", 600)
	fetched := ScopeState{Label: "team t1", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{fetchedFeed("models-dev")}},
		States: map[string]*StateRecord{"models-dev": freshState("models-dev", now, pricedIDs(fetchedIDs))}}
	bigDeclared := ScopeState{Label: "team t1", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{declaredFeed("team-rows", declaredDoc(randomIDs(t, "vendor/", 1000)))}}}
	for _, team := range []ScopeState{fetched, bigDeclared} {
		if Fold(FoldConfig{Now: now, Budget: 1 << 30}, platform, team).Fits(DefaultBudget) {
			t.Fatal("the whole table fits the default budget — the case does not cross it")
		}
	}

	for _, tc := range []struct {
		name        string
		team        ScopeState
		named       []string
		wantEntries []string
		wantWarning string
	}{
		{"a dial run beside a fetched feed", fetched, nil, []string{deepseek, glm},
			"prices narrowed to 2 ids — 0 the run names, 1 with accepted efforts, 1 declared — accepted efforts kept for all 1 ids"},
		{"a run naming a fetched id", fetched, []string{fetchedIDs[0]}, []string{deepseek, glm, fetchedIDs[0]},
			"prices narrowed to 3 ids — 1 the run names, 1 with accepted efforts, 1 declared — accepted efforts kept for all 1 ids"},
		{"a dial run beside a declaration over budget", bigDeclared, nil, []string{deepseek},
			"prices narrowed to 1 ids — 0 the run names, 1 with accepted efforts, 0 declared — accepted efforts kept for all 1 ids"},
		{"a run naming glm beside a declaration over budget", bigDeclared, []string{glm}, []string{deepseek, glm},
			"prices narrowed to 2 ids — 1 the run names, 1 with accepted efforts, 0 declared — accepted efforts kept for all 1 ids"},
	} {
		var warned []string
		got := Fold(FoldConfig{Now: now, Named: tc.named, Warnf: func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }}, platform, tc.team)
		if got == nil || !got.Fits(DefaultBudget) {
			t.Fatalf("[%s] the fold answered %v, want a table within the budget", tc.name, got != nil)
		}
		var entries []string
		for id := range got.Entries {
			entries = append(entries, id)
		}
		sort.Strings(entries)
		want := slices.Clone(tc.wantEntries)
		sort.Strings(want)
		if !slices.Equal(entries, want) {
			t.Errorf("[%s] the narrowed prices are %d ids %.200v, want %v", tc.name, len(entries), entries, want)
		}
		if e := got.Entries[deepseek]; e.Spec.ContextWindow != 256000 || e.Spec.MaxOutputTokens != 32768 || e.Spec.InputCostPerM != 0.672 {
			t.Errorf("[%s] deepseek's entry is %+v, want the declaration's whole", tc.name, e.Spec)
		}
		if e := got.Efforts[deepseek]; e.Feed != "pliage" || len(e.Values) != 4 {
			t.Errorf("[%s] deepseek's efforts are %+v, want the declaration's four levels", tc.name, e)
		}
		if len(warned) != 1 || !strings.Contains(warned[0], tc.wantWarning) {
			t.Errorf("[%s] the narrowing warned %q, want one line saying %q", tc.name, warned, tc.wantWarning)
		}
	}
}

// When the prices of the ids with accepted efforts do not fit beside the
// rest, they step down too, and the named ids keep theirs: the efforts
// themselves still travel for every id.
func TestFoldOverBudgetGivesUpTheEffortsIDsPricesBeforeTheNamedOnes(t *testing.T) {
	now := time.Now()
	ids := randomIDs(t, "gw/", 600)
	entries := pricedIDs(ids)
	for id, spec := range entries {
		spec.ReasoningEfforts = []string{"low", "high"}
		entries[id] = spec
	}
	scope := ScopeState{Label: "platform", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{fetchedFeed("exec-gw")}},
		States: map[string]*StateRecord{"exec-gw": freshState("exec-gw", now, entries)}}
	var warned []string
	got := Fold(FoldConfig{Now: now, Named: ids[:1], Warnf: func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }}, scope)
	if got == nil || !got.Fits(DefaultBudget) {
		t.Fatal("the fold answered no table within the budget")
	}
	if len(got.Entries) != 1 || len(got.Efforts) != 600 {
		t.Fatalf("the narrowed table carries %d prices and %d effort lists, want the named id's price and all 600 lists", len(got.Entries), len(got.Efforts))
	}
	if _, ok := got.Entries[ids[0]]; !ok {
		t.Fatalf("the named id's price was dropped")
	}
	if want := "prices narrowed to 1 ids — 1 the run names, 0 with accepted efforts, 0 declared — accepted efforts kept for all 600 ids"; len(warned) != 1 || !strings.Contains(warned[0], want) {
		t.Fatalf("the narrowing warned %q, want %q", warned, want)
	}
}
