package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The `_gateway_spec` record carries the feed's identity AND age when a
// feed answered (ADR-163 §6) — the audit trail #3103 established,
// extended, not forked.
func TestGatewaySpecRecord_CarriesFeedAge(t *testing.T) {
	refreshed := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Second)
	res := compatgw.Resolved{
		Spec:        modelspecs.Spec{InputCostPerM: 0.42, OutputCostPerM: 1.4, CacheReadCostPerM: 0.08},
		Source:      compatgw.SourceFeed("scw"),
		RefreshedAt: refreshed,
	}
	raw := gatewaySpecRecord(res)
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["source"] != "feed:scw" {
		t.Fatalf("the record's source is %v", record["source"])
	}
	if got, ok := record["refreshed_at"].(string); !ok || !strings.HasPrefix(got, refreshed.Format("2006-01-02T")) {
		t.Fatalf("the record's feed age is %v", record["refreshed_at"])
	}
	if record["cache_read_usd_per_mtok"].(float64) != 0.08 {
		t.Fatalf("the cache rate #3104 added did not survive: %v", record)
	}
	// A non-feed record carries no age key at all.
	operator := compatgw.Resolved{Spec: res.Spec, Source: compatgw.SourceOperator}
	record = map[string]any{}
	if err := json.Unmarshal(gatewaySpecRecord(operator), &record); err != nil {
		t.Fatal(err)
	}
	if _, has := record["refreshed_at"]; has {
		t.Fatal("a non-feed record carries refreshed_at")
	}
}

// The crossing forwards the run's feed table beside the IPC record — the
// explicit empty when the run carries none (the workspace .env cannot
// fill the name), the overlay's bytes when it does, and the record the
// host crosses is resolved from the SAME environment.
func TestForwardableProviderEnv_CrossesTheFeedTable(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	t.Setenv(compatgw.CatalogModelsEnv, "")
	t.Setenv(compatgw.CatalogProviderEnv, "")
	t.Setenv(compatgw.ResolvedEnvName, "")
	compatgw.ResetCatalogCaches()
	ctx := context.Background()
	env, _, err := forwardableProviderEnv(ctx, modelroute.OpenAICompatible+"/glm-5.3", "")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := env[compatgw.FeedTableEnv]; !ok || v != "" {
		t.Fatalf("the feed name must cross as an explicit empty: %q %v", v, ok)
	}

	table := pricefeed.Table{Entries: map[string]pricefeed.Entry{
		"glm-5.3": {
			Feed: "scw", RefreshedAt: time.Now(), FreshFor: pricefeed.D(2 * time.Hour),
			Spec: modelspecs.Spec{InputCostPerM: 0.42, OutputCostPerM: 1.4},
		},
	}}
	raw, _ := json.Marshal(table)
	ctx = compatgw.OverlayFeedTable(ctx, raw)
	env, _, err = forwardableProviderEnv(ctx, modelroute.OpenAICompatible+"/glm-5.3", "")
	if err != nil {
		t.Fatal(err)
	}
	if env[compatgw.FeedTableEnv] != string(raw) {
		t.Fatal("the run's feed table did not cross")
	}
	var record compatgw.ResolvedEnvRecord
	if err := json.Unmarshal([]byte(env[compatgw.ResolvedEnvName]), &record); err != nil {
		t.Fatal(err)
	}
	if record.Source != compatgw.SourceFeed("scw") || record.InputUSDPerMTok != 0.42 {
		t.Fatalf("the crossed record did not resolve from the feed: %+v", record)
	}
	if record.RefreshedAt == nil {
		t.Fatal("the crossed record carries no feed age")
	}
	// The overlay answers the context's readers without touching the
	// process env: a sibling context sees nothing.
	if v := credroute.Getenv(context.Background(), compatgw.FeedTableEnv); v != "" {
		t.Fatal("the overlay leaked to a context it was not pinned on")
	}
}

// The coarse walk that feeds the fold's narrowing fallback: primary
// models, fallbacks (own model and inherited), the human/interaction
// half and the recovery rungs; only gateway-prefixed ids answer.
func TestRouteGatewayModelIDs(t *testing.T) {
	node := &ir.AgentNode{}
	node.ID = "review"
	node.Model = modelroute.OpenAICompatible + "/glm-5.3"
	node.Fallbacks = []ir.Fallback{
		{Name: "f1", Model: modelroute.OpenAICompatible + "/gpt-oss-120b"},
		{Name: "f2"}, // inherits the node's model
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"review": node}}
	ids := RouteGatewayModelIDs(wf)
	want := map[string]bool{"glm-5.3": true, "gpt-oss-120b": true}
	if len(ids) != len(want) {
		t.Fatalf("ids %v, want exactly %v", ids, want)
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id %q in %v", id, ids)
		}
	}
	if RouteGatewayModelIDs(nil) != nil {
		t.Fatal("a nil workflow yielded ids")
	}
}
