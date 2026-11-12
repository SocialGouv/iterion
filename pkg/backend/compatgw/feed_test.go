package compatgw

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// feedTableJSON builds the wire form of a one-entry feed table.
func feedTableJSON(t *testing.T, feed string, age time.Duration, freshFor time.Duration, in, out float64) string {
	t.Helper()
	raw, err := json.Marshal(pricefeed.Table{Entries: map[string]pricefeed.Entry{
		"glm-5.3": {
			Feed: feed, RefreshedAt: time.Now().Add(-age), FreshFor: pricefeed.Duration(freshFor),
			Spec: modelspecs.Spec{InputCostPerM: in, OutputCostPerM: out},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// ADR-163's precedence rung: a FRESH feed entry answers before the
// operator table for the ids it carries, carrying the feed's identity
// The host IPC record still answers FIRST: agreement across the IPC beats
// any local freshness, feed table included — the record is the host's own
// per-invocation answer, whatever table it resolved from.
func TestResolveCatalog_RecordBeatsFeedTable(t *testing.T) {
	ResetCatalogCaches()
	t.Setenv(CatalogModelsEnv, "")
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(FeedTableEnv, feedTableJSON(t, "scw", time.Minute, 2*time.Hour, 0.6, 1.8))
	record, err := json.Marshal(ResolvedEnvRecord{
		Model: "glm-5.3", Source: SourceOperator,
		InputUSDPerMTok: 4, OutputUSDPerMTok: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ResolvedEnvName, string(record))
	res := ResolveCatalog("glm-5.3", nil, os.Getenv)
	if res.Source != SourceOperator || res.Spec.InputCostPerM != 4 {
		t.Fatalf("the record did not answer before the feed table: %+v", res)
	}
}

// A record sourced from a feed crosses and re-reads — with its age.
func TestForwardableRecord_CarriesFeedProvenanceAndAge(t *testing.T) {
	refreshed := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Second)
	res := Resolved{
		Spec:        modelspecs.Spec{InputCostPerM: 0.6, OutputCostPerM: 1.8},
		Source:      SourceFeed("scw"),
		RefreshedAt: refreshed,
	}
	rec := ForwardableRecord("glm-5.3", res)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ResolvedEnvName, string(raw))
	t.Setenv(CatalogModelsEnv, `{"glm-5.3":{"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(CatalogProviderEnv, "")
	ResetCatalogCaches()
	back := ResolveCatalog("glm-5.3", nil, os.Getenv)
	if back.Source != SourceFeed("scw") || back.Spec.InputCostPerM != 0.6 {
		t.Fatalf("the feed record did not survive the IPC round trip: %+v", back)
	}
	if !back.RefreshedAt.Equal(refreshed) {
		t.Fatalf("the feed's age did not cross: %v vs %v", back.RefreshedAt, refreshed)
	}
	// A fabricated source still refuses to pass as a record — the feed
	// vocabulary widens the validation by its PREFIX, not to anything.
	if raw, err := json.Marshal(ResolvedEnvRecord{Model: "m", Source: "wizard"}); err == nil {
		t.Setenv(ResolvedEnvName, string(raw))
		if _, ok := resolvedEnvRecord(os.Getenv); ok {
			t.Fatal("a fabricated source passed as a record")
		}
	}
	// A feed-named record without its age still answers — priced and
	// provenance-carried, the age simply unknown.
	if raw, err := json.Marshal(ResolvedEnvRecord{Model: "m", Source: SourceFeed("scw"), InputUSDPerMTok: 1, OutputUSDPerMTok: 2}); err == nil {
		t.Setenv(ResolvedEnvName, string(raw))
		rec, ok := resolvedEnvRecord(os.Getenv)
		if !ok || rec.Source != SourceFeed("scw") {
			t.Fatal("a feed-named record refused")
		}
	}
}

// The planted filter: a feed table a project `.env` planted prices
// nothing — the name is host-written, and one no host wrote reads as no
// table at all.
func TestFeedTable_PlantedValueRefused(t *testing.T) {
	inheritEnv(t)
	t.Setenv(CatalogModelsEnv, `{"glm-5.3":{"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(ResolvedEnvName, "")
	t.Setenv(FeedTableEnv, feedTableJSON(t, "scw", time.Minute, 2*time.Hour, 0.6, 1.8))
	plantEnv(t, FeedTableEnv)
	res := ResolveCatalog("glm-5.3", nil, os.Getenv)
	if res.Source != SourceOperator || res.Spec.InputCostPerM != 9 {
		t.Fatalf("a planted feed table priced a node: %+v", res)
	}
}

// A garbage feed table degrades to the local sources; it never invents
// one.
func TestFeedTable_GarbageIgnored(t *testing.T) {
	ResetCatalogCaches()
	t.Setenv(CatalogModelsEnv, `{"glm-5.3":{"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(ResolvedEnvName, "")
	t.Setenv(FeedTableEnv, "{{not json")
	res := ResolveCatalog("glm-5.3", nil, os.Getenv)
	if res.Source != SourceOperator {
		t.Fatalf("garbage did not degrade to the operator table: %+v", res)
	}
}

// The bundle→context adapter: valid bytes plant the name the catalog
// reads; malformed bytes plant nothing (the run keeps the local sources).
func TestOverlayFeedTable(t *testing.T) {
	good := feedTableJSON(t, "scw", time.Minute, 2*time.Hour, 0.6, 1.8)
	ctx := OverlayFeedTable(context.Background(), []byte(good))
	if got := credroute.Getenv(ctx, FeedTableEnv); got != good {
		t.Fatal("the overlay did not plant the wire bytes")
	}
	if got := credroute.Getenv(ctx, "ANYTHING_ELSE"); got != "" {
		t.Fatal("the overlay answered a name it does not hold — it must add, never replace")
	}
	if overlayNil := OverlayFeedTable(context.Background(), []byte("garbage")); credroute.Getenv(overlayNil, FeedTableEnv) != "" {
		t.Fatal("malformed bytes planted a table")
	}
	if same := OverlayFeedTable(context.Background(), nil); credroute.Getenv(same, FeedTableEnv) != "" {
		t.Fatal("empty bytes planted a table")
	}
}

// The pricer reads the run's feed table through the SAME get the
// resolution does — the stamp and the price cannot disagree (the #3104
// class, closed at the seam).
func TestStaticPricer_PricesFromTheFeedTable(t *testing.T) {
	ResetCatalogCaches()
	t.Setenv(CatalogModelsEnv, `{"glm-5.3":{"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(ResolvedEnvName, "")
	feed := feedTableJSON(t, "scw", time.Minute, 2*time.Hour, 0.6, 1.8)
	ctx := credroute.WithEnvOverlay(context.Background(), FeedTableEnv, feed)

	in, out, _, known := StaticPricer("glm-5.3", func(name string) string { return credroute.Getenv(ctx, name) })
	if !known || in != 0.6 || out != 1.8 {
		t.Fatalf("the pricer did not read the feed table: in=%v out=%v known=%v", in, out, known)
	}
	// The env-less form reads the process: the operator floor, as before.
	in, _, _, known = StaticPricer("glm-5.3", os.Getenv)
	if !known || in != 9 {
		t.Fatalf("the process read did not reach the operator table: in=%v known=%v", in, known)
	}
}

// The staleness step-down says itself once per feed per process on the
// PRESENCE IS THE VERDICT: freshness was judged once, at the fold, by
// the server; the table that travels answers for the invocation's whole
// life. A window lapsing mid-run changes nothing (ADR-122's
// "never read again in mid-flight"); a NEW raw value — the next run's
// fold, judged by the server — is a different table.
func TestResolveCatalog_TheFoldedTableAnswersForTheInvocation(t *testing.T) {
	ResetCatalogCaches()
	t.Setenv(CatalogModelsEnv, `{"glm-5.3":{"input_usd_per_mtok":9,"output_usd_per_mtok":9},"glm-5.2":{"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(ResolvedEnvName, "")
	// A window of one second: it lapses mid-test, the answer does not.
	t.Setenv(FeedTableEnv, feedTableJSON(t, "scw", 0, time.Second, 0.6, 1.8))

	first := ResolveCatalog("glm-5.3", nil, os.Getenv)
	if first.Source != SourceFeed("scw") || first.Spec.InputCostPerM != 0.6 {
		t.Fatalf("the folded entry did not answer: %+v", first)
	}
	time.Sleep(1500 * time.Millisecond)
	after := ResolveCatalog("glm-5.3", nil, os.Getenv)
	if after.Source != SourceFeed("scw") || after.Spec.InputCostPerM != 0.6 {
		t.Fatalf("the answer changed mid-invocation: %+v", after)
	}
	// Memo eviction re-parses the same value: the same answer, no clock.
	ResetFeedTableCache()
	reParsed := ResolveCatalog("glm-5.3", nil, os.Getenv)
	if reParsed.Source != SourceFeed("scw") || reParsed.Spec.InputCostPerM != 0.6 {
		t.Fatalf("a re-parse changed the answer: %+v", reParsed)
	}
	// An id the table does not carry keeps the operator floor.
	other := ResolveCatalog("glm-5.2", nil, os.Getenv)
	if other.Source != SourceOperator {
		t.Fatalf("an id outside the feed table did not reach the operator table: %+v", other)
	}
}

// Whole-entry precedence at the feed rung: a feed entry that prices an id
// and states no window or output cap answers WHOLE over the operator
// table's entry for that id — the operator's window and output cap are
// not merged in.
func TestResolveCatalog_AFeedEntryAnswersWholeOverTheOperatorTable(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	get := mapGet(map[string]string{
		CatalogModelsEnv: `{"glm-5.3":{"context_window":256000,"max_output_tokens":32768,"input_usd_per_mtok":0.672,"output_usd_per_mtok":2.016,"cache_read_usd_per_mtok":0.1344}}`,
		FeedTableEnv:     feedTableJSON(t, "pliage-litellm", time.Minute, time.Hour, 0.7, 2.1),
	})
	res := ResolveCatalog("glm-5.3", nil, get)
	if res.Source != SourceFeed("pliage-litellm") || res.Spec.InputCostPerM != 0.7 || res.Spec.OutputCostPerM != 2.1 {
		t.Fatalf("the feed entry did not answer: %+v", res)
	}
	if res.Spec.ContextWindow != 0 || res.Spec.MaxOutputTokens != 0 || res.Spec.CacheReadCostPerM != 0 {
		t.Fatalf("the operator table's fields were merged into the feed's answer: %+v", res.Spec)
	}
}
