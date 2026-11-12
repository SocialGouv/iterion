package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The feed view names the params it does not render as stored — a
// credential-shaped key's value, a URL stripped of its userinfo, query or
// fragment — so a client that writes a read list back knows which values it
// does not hold. A param rendered as stored is not named.
func TestPriceFeedViewNamesTheParamsItRedacts(t *testing.T) {
	s, _ := priceFeedFixtureServer(t)
	docID := pricefeed.TeamConfigDocID("team1")
	hourly := func(name, typ string, params map[string]string) pricefeed.FeedConfig {
		return pricefeed.FeedConfig{Name: name, Type: typ, Enabled: true,
			Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(10 * time.Second), Params: params}
	}
	feeds := []pricefeed.FeedConfig{
		hourly("gw", pricefeed.TypeLiteLLM, map[string]string{"url": "https://gw.example/model/info", "token": "tenant-litellm-bearer"}),
		hourly("tariffs", pricefeed.TypeModelsDev, map[string]string{"provider": "scaleway", "url": "https://ops:pw@tariffs.example/api.json?token=SEKRIT"}),
		hourly("public", pricefeed.TypeModelsDev, map[string]string{"provider": "scaleway", "url": "https://models.dev/api.json"}),
		declaredFeed("pliage", pliageEntries),
	}
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: feeds}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleGetTeamPriceFeeds(w, priceFeedReq(asUser("u-teamadmin"), "GET", "/api/teams/team1/price-feeds", ""))
	var view struct {
		Feeds []struct {
			Feed           pricefeed.FeedConfig `json:"feed"`
			RedactedParams []string             `json:"redacted_params"`
		} `json:"feeds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode the view: %v (%s)", err, w.Body.String())
	}
	want := map[string][]string{"gw": {"token"}, "tariffs": {"url"}, "public": nil, "pliage": nil}
	if len(view.Feeds) != len(want) {
		t.Fatalf("the view answers %d feeds, want %d: %s", len(view.Feeds), len(want), w.Body.String())
	}
	for _, f := range view.Feeds {
		if !slices.Equal(f.RedactedParams, want[f.Feed.Name]) {
			t.Errorf("feed %s: redacted_params %q, want %q", f.Feed.Name, f.RedactedParams, want[f.Feed.Name])
		}
	}
}
