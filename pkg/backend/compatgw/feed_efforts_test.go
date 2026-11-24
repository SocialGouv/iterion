package compatgw

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// An efforts-only declaration never shadows a price: the folded table
// carries the efforts in their own map, so the catalog's feed rung finds
// no price entry for the id and the operator table answers with its own
// prices.
func TestFeedEffortsOnlyNeverShadowsTheOperatorPrice(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	now := time.Now()
	table := pricefeed.Fold(pricefeed.FoldConfig{Now: now}, pricefeed.ScopeState{Config: &pricefeed.ConfigRecord{
		UpdatedAt: now,
		Feeds: []pricefeed.FeedConfig{{
			Name: "pliage", Type: pricefeed.TypeDeclared, Enabled: true,
			Interval: pricefeed.D(30 * 24 * time.Hour), Timeout: pricefeed.D(time.Second),
			Params: map[string]string{"entries": `{"scaleway/deepseek-x":{"reasoning_efforts":["low","high","xhigh","max"]}}`},
		}},
	}})
	if table == nil {
		t.Fatal("the efforts-only declaration folded to nothing")
	}
	raw, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	get := mapGet(map[string]string{
		FeedTableEnv:     string(raw),
		CatalogModelsEnv: `{"scaleway/deepseek-x":{"context_window":128000,"input_usd_per_mtok":0.3,"output_usd_per_mtok":1.2}}`,
	})
	res := ResolveCatalogQuiet("scaleway/deepseek-x", nil, get)
	if res.Source != SourceOperator || res.Spec.InputCostPerM != 0.3 || res.Spec.OutputCostPerM != 1.2 || res.Spec.ContextWindow != 128000 {
		t.Fatalf("an efforts-only declaration shadowed the operator price: %+v", res)
	}
}
