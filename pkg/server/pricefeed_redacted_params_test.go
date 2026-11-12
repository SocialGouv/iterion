package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// Every write route refuses a list holding the view's redaction marker as a
// param's value — the platform's, an org's and a team's — answering 400
// that names the feed and the param, and leaves the stored credential as
// it was. The same list with the stored value writes.
func TestPriceFeedWriteRefusesTheRedactionMarker(t *testing.T) {
	live := newPriceFeedLive(t)
	stored := pricefeed.FeedConfig{Name: "gw", Type: pricefeed.TypeLiteLLM, Enabled: true,
		Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(10 * time.Second),
		Params: map[string]string{"url": "https://gw.example/model/info", "token": "sk-litellm-REAL-BEARER"}}
	readBack := stored
	readBack.Params = map[string]string{"url": stored.Params["url"], "token": pricefeed.RedactedParamValue}
	for _, route := range []struct {
		name, path, docID string
		token             func() string
	}{
		{"platform", "/api/admin/price-feeds", pricefeed.PlatformConfigDocID, func() string { return live.root }},
		{"org", "/api/orgs/org1/price-feeds", pricefeed.OrgConfigDocID("org1"), func() string { return live.orgAdmin }},
		{"team", "/api/teams/team-pf/price-feeds", pricefeed.TeamConfigDocID("team-pf"), func() string { return live.orgAdmin }},
	} {
		t.Run(route.name, func(t *testing.T) {
			ctx := context.Background()
			if err := live.configs.Put(ctx, route.docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{stored}, UpdatedBy: "seed"}); err != nil {
				t.Fatal(err)
			}
			before, err := live.configs.Get(ctx, route.docID)
			if err != nil || before == nil {
				t.Fatalf("read the seed: %v", err)
			}
			code, out := live.do(t, "PUT", route.path, route.token(), priceFeedPutFeedBody(readBack))
			if code != http.StatusBadRequest || !strings.Contains(string(out), `feed \"gw\": params token hold`) {
				t.Fatalf("a list holding the marker answered %d %s, want 400 naming feed gw and param token", code, out)
			}
			after, err := live.configs.Get(ctx, route.docID)
			if err != nil || after == nil || after.Feeds[0].Params["token"] != "sk-litellm-REAL-BEARER" || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Fatalf("the refused write moved the stored record: %+v (err %v), was %+v", after, err, before)
			}
			if code, out := live.do(t, "PUT", route.path, route.token(), priceFeedPutFeedBody(stored)); code != http.StatusOK {
				t.Fatalf("the list with the stored value answered %d %s", code, out)
			}
		})
	}
}
