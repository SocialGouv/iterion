package server

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The fold the server binary hands the publisher
// (PublisherPriceFeedEffective) reads the org and the team the publisher
// passes, in that order, narrows an over-budget table by the run's named
// ids, and says so to its warnf. The org and the team each answer an id's
// effort list the other does not, and the platform's declaration is wide
// enough to exceed the run channel's budget: a fold that swaps the org and
// the team, drops either, drops the named ids or drops the warnf answers
// another table or warns no one.
func TestPublisherPriceFeedEffectiveFoldsTheOrgAndTheTeamItIsGiven(t *testing.T) {
	configs, states := pricefeed.NewMemoryConfigStore(), pricefeed.NewMemoryStateStore()
	var wide strings.Builder
	wide.WriteString("{")
	for i := range 1000 {
		if i > 0 {
			wide.WriteString(",")
		}
		fmt.Fprintf(&wide, `"wide-%04d":{"input_usd_per_mtok":1,"output_usd_per_mtok":2}`, i)
	}
	wide.WriteString("}")
	putFeedDoc(t, configs, pricefeed.PlatformConfigDocID, declaredFeed("wide", wide.String()))
	putFeedDoc(t, configs, pricefeed.OrgConfigDocID("org1"), declaredFeed("org-decl",
		`{"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high"]},"glm-5.3":{"reasoning_efforts":["low","max"]}}`))
	putFeedDoc(t, configs, pricefeed.TeamConfigDocID("team1"), declaredFeed("team-decl",
		`{"glm-5.3":{"reasoning_efforts":["high","max"]}}`))

	var mu sync.Mutex
	var warnings []string
	fold := PublisherPriceFeedEffective(configs, states, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	raw := mustTable(t)(fold(context.Background(), "org1", "team1", []string{"wide-0007"}))
	table := pricefeed.ParseTable(string(raw))
	if table == nil {
		t.Fatalf("the publisher's fold answered no table: %.200s", raw)
	}
	want := map[string]pricefeed.EffortsEntry{
		"scaleway/deepseek-v4.1-flash": {Feed: "org-decl", Values: []string{"low", "high"}},
		"glm-5.3":                      {Feed: "team-decl", Values: []string{"high", "max"}},
	}
	if !reflect.DeepEqual(table.Efforts, want) {
		t.Errorf("efforts = %+v, want deepseek from the org and glm-5.3 from the team: %+v", table.Efforts, want)
	}
	if ids := slices.Sorted(maps.Keys(table.Entries)); !reflect.DeepEqual(ids, []string{"wide-0007"}) {
		t.Errorf("prices answer %d ids (first %v), want only the run's named wide-0007", len(ids), ids[:min(5, len(ids))])
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(warnings, func(w string) bool {
		return strings.Contains(w, "prices narrowed to 1 ids — 1 the run names, 0 with accepted efforts, 0 declared — accepted efforts kept for all 2 ids")
	}) {
		t.Errorf("the narrowing reached no warning: %q", warnings)
	}
}
