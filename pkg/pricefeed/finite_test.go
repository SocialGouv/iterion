package pricefeed

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// A source answering a price no wire can carry fails its refresh, naming
// the id: a LiteLLM per-token cost of 1e303 scales to +Inf per mtok. The
// previous entries stay, aging as any failing feed's do, and the outcome
// says which id and which field.
func TestRefreshRefusesAPriceThatIsNotFinite(t *testing.T) {
	stubHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"model_name":"glm-5.3","model_info":{"input_cost_per_token":6.1e-7,"output_cost_per_token":1.8e-6}},
			{"model_name":"bogus","model_info":{"input_cost_per_token":1e303,"output_cost_per_token":1e-6}}
		]}`))
	})
	now := time.Now()
	feed := FeedConfig{Name: "gw", Type: TypeLiteLLM, Enabled: true, Interval: d(3600), Timeout: d(10), Params: map[string]string{"url": os.Getenv("PRICEFEED_TEST_URL")}}
	prev := &StateRecord{Feed: "gw", Type: TypeLiteLLM, Entries: map[string]modelspecs.Spec{"glm-5.3": {InputCostPerM: 0.5, OutputCostPerM: 1.5}}, RefreshedAt: now.Add(-time.Minute), FreshFor: d(7200)}
	state, outcome := Refresh(context.Background(), feed, prev, nil, now)
	if outcome.Status != StateFailing || !strings.Contains(outcome.Error, `entry "bogus": input_usd_per_mtok is +Inf, not a finite price`) {
		t.Fatalf("outcome = %+v, want failing, naming bogus's input price", outcome)
	}
	if e := state.Entries["glm-5.3"]; e.InputCostPerM != 0.5 || len(state.Entries) != 1 {
		t.Fatalf("the failing refresh kept %+v, want the previous entries", state.Entries)
	}

	const nanType = "test-nan-source"
	RegisterSource(nanType, stubSource{entries: map[string]modelspecs.Spec{"a": {InputCostPerM: 1, OutputCostPerM: 2, CacheReadCostPerM: math.NaN()}}})
	t.Cleanup(func() {
		sourceMu.Lock()
		defer sourceMu.Unlock()
		delete(sources, nanType)
	})
	nan := FeedConfig{Name: "nan", Type: nanType, Enabled: true, Interval: d(3600), Timeout: d(10)}
	if _, outcome := Refresh(context.Background(), nan, nil, nil, now); outcome.Status != StateFailing || !strings.Contains(outcome.Error, `entry "a": cache_read_usd_per_mtok is NaN`) {
		t.Fatalf("a NaN cache price refreshed %+v, want it named", outcome)
	}
}

// stubSource answers fixed entries.
type stubSource struct{ entries map[string]modelspecs.Spec }

func (stubSource) ValidateParams(map[string]string) error { return nil }
func (s stubSource) Fetch(context.Context, map[string]string) (map[string]modelspecs.Spec, error) {
	return s.entries, nil
}

// A fold over a state holding a non-finite price — one stored by a release
// whose refresh did not refuse it — leaves that entry out, named, and
// answers the rest: the other ids' prices and every accepted effort, the
// bogus entry's own included. A table holding it would not encode, and
// would take every id of the run with it.
func TestFoldLeavesOutAnEntryWhosePriceIsNotFinite(t *testing.T) {
	now := time.Now()
	var warned []string
	warnf := func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }
	feed := fetchedFeed("gw")
	states := map[string]*StateRecord{"gw": freshState("gw", now, map[string]modelspecs.Spec{
		"glm-5.3": {InputCostPerM: 0.5, OutputCostPerM: 1.5},
		"bogus":   {InputCostPerM: math.Inf(1), OutputCostPerM: 1, ReasoningEfforts: []string{"low", "high"}},
	})}
	table := Fold(FoldConfig{Now: now, Named: []string{"bogus"}, Warnf: warnf}, ScopeState{Label: "platform", Config: &ConfigRecord{UpdatedAt: now, Feeds: []FeedConfig{feed}}, States: states})
	if table == nil || table.Entries["glm-5.3"].Spec.InputCostPerM != 0.5 {
		t.Fatalf("the finite entry does not answer: %+v", table)
	}
	if _, here := table.Entries["bogus"]; here {
		t.Fatalf("the non-finite entry answered: %+v", table.Entries["bogus"])
	}
	if got := table.Efforts["bogus"]; got.Feed != "gw" || strings.Join(got.Values, ",") != "low,high" {
		t.Errorf("the bogus entry's efforts answer %+v, want gw's", got)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], `price feed gw (platform): entry "bogus": input_usd_per_mtok is +Inf, not a finite price`) {
		t.Fatalf("warnings %q, want one naming the entry", warned)
	}
}
