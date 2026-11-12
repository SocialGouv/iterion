package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// priceFeedEffectiveWire decodes the effective view by its JSON names.
type priceFeedEffectiveWire struct {
	TeamID string    `json:"team_id"`
	OrgID  string    `json:"org_id"`
	At     time.Time `json:"at"`
	Scopes []struct {
		Scope  string   `json:"scope"`
		ID     string   `json:"id"`
		Feeds  []string `json:"feeds"`
		Unread string   `json:"unread"`
	} `json:"scopes"`
	Models []struct {
		ID           string                  `json:"id"`
		Entry        *pricefeed.Entry        `json:"entry"`
		Efforts      *pricefeed.EffortsEntry `json:"efforts"`
		Contributors []struct {
			Scope          string           `json:"scope"`
			ScopeID        string           `json:"scope_id"`
			Feed           string           `json:"feed"`
			Spec           *modelspecs.Spec `json:"spec"`
			Efforts        []string         `json:"efforts"`
			AnswersEntry   bool             `json:"answers_entry"`
			AnswersEfforts bool             `json:"answers_efforts"`
		} `json:"contributors"`
	} `json:"models"`
	Table             json.RawMessage `json:"table"`
	Warnings          []string        `json:"warnings"`
	RunnerEnvFallback string          `json:"runner_env_fallback"`
}

// putFeedDoc stores one level's feed list straight into the store — the
// route's validation is not this test's subject (a broken declaration
// is stored this way on purpose).
func putFeedDoc(t *testing.T, configs pricefeed.ConfigStore, docID string, feeds ...pricefeed.FeedConfig) {
	t.Helper()
	if err := configs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: feeds}); err != nil {
		t.Fatal(err)
	}
}

// The effective read model: member-readable (a non-member is refused),
// the org resolved from the team, its table the very bytes the
// publisher's fold seals for the same team and ids, and per id the
// answering entry and efforts beside every level and feed stating it —
// judged by value, so two feeds of one name at two levels are told
// apart. The fold's warnings and the runner's unseen fallback ride along.
func TestPriceFeedEffectiveView(t *testing.T) {
	live := newPriceFeedLive(t)
	// The platform and the org both name their feed "pliage": the answer
	// is told by value, never by the name.
	putFeedDoc(t, live.configs, "platform", declaredFeed("pliage",
		`{"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]},"kimi-k2":{"reasoning_efforts":["high"]}}`))
	putFeedDoc(t, live.configs, pricefeed.OrgConfigDocID("org1"), declaredFeed("pliage",
		`{"glm-5.3":{"input_usd_per_mtok":2.5,"output_usd_per_mtok":7}}`))
	putFeedDoc(t, live.configs, pricefeed.TeamConfigDocID("team-pf"),
		declaredFeed("teamf", `{"kimi-k2":{"reasoning_efforts":["max"]}}`),
		declaredFeed("broken", `{"glm-5.3":{}}`))

	const base = "/api/teams/team-pf/price-feeds/effective"
	query := "?model=glm-5.3&model=Scaleway/DeepSeek-v4.1-flash&model=kimi-k2&model=unknown-x"
	code, out := live.do(t, "GET", base+query, live.viewer, "")
	if code != http.StatusOK {
		t.Fatalf("a team viewer cannot read the effective feed: %d %s", code, out)
	}
	if code, out := live.do(t, "GET", base+query, live.outsider, ""); code != http.StatusForbidden {
		t.Fatalf("a non-member read the team's effective feed: %d %s", code, out)
	}
	var view priceFeedEffectiveWire
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}

	// The table is the publisher's: the same function, the same team,
	// org and ids — whitespace aside, the same bytes.
	sealed := EffectivePriceFeed(context.Background(), live.configs, live.states, "org1", "team-pf",
		[]string{"glm-5.3", "Scaleway/DeepSeek-v4.1-flash", "kimi-k2", "unknown-x"}, nil)
	if sealed == nil {
		t.Fatal("the publisher's fold answered no table")
	}
	var gotTable, wantTable bytes.Buffer
	if err := json.Compact(&gotTable, view.Table); err != nil {
		t.Fatalf("the view's table is not JSON: %v (%s)", err, view.Table)
	}
	if err := json.Compact(&wantTable, sealed); err != nil {
		t.Fatal(err)
	}
	if gotTable.String() != wantTable.String() {
		t.Fatalf("the view's table differs from the sealed one:\n view:   %s\n sealed: %s", gotTable.String(), wantTable.String())
	}

	if view.TeamID != "team-pf" || view.OrgID != "org1" || view.RunnerEnvFallback != "not visible from the server" {
		t.Fatalf("the view's frame = team %q org %q fallback %q", view.TeamID, view.OrgID, view.RunnerEnvFallback)
	}
	var levels []string
	for _, sc := range view.Scopes {
		levels = append(levels, sc.Scope+":"+sc.ID+"="+strings.Join(sc.Feeds, ","))
	}
	if got := strings.Join(levels, " "); got != "platform:=pliage org:org1=pliage team:team-pf=teamf,broken" {
		t.Fatalf("the levels read = %q", got)
	}
	if !strings.Contains(strings.Join(view.Warnings, "\n"), "broken") {
		t.Fatalf("the fold's warning about the broken declaration is missing: %q", view.Warnings)
	}
	// The org's glm-5.3 answers whole over the platform's and states no
	// window or output cap: the view says so, naming both levels.
	if want := "price feed pliage (org org1) answers whole over feed pliage (platform) and leaves context_window, max_output_tokens unstated for 1 ids (glm-5.3)"; !strings.Contains(strings.Join(view.Warnings, "\n"), want) {
		t.Fatalf("the shadowing warning is missing from the view: %q", view.Warnings)
	}

	if len(view.Models) != 4 {
		t.Fatalf("the view reports %d ids, want the 4 requested: %s", len(view.Models), out)
	}
	byID := map[string]int{}
	for i, m := range view.Models {
		byID[m.ID] = i
	}
	type contributor struct {
		level, feed    string
		entry, efforts bool
		effortsValues  string
		answersEntry   bool
		answersEfforts bool
	}
	check := func(id string, wantEntryFeed string, wantIn float64, wantEffortsFeed, wantEfforts string, want []contributor) {
		t.Helper()
		i, ok := byID[id]
		if !ok {
			t.Fatalf("id %s is not reported: %s", id, out)
		}
		m := view.Models[i]
		switch {
		case wantEntryFeed == "" && m.Entry != nil:
			t.Errorf("%s: an entry answers (%+v), want none", id, m.Entry)
		case wantEntryFeed != "" && (m.Entry == nil || m.Entry.Feed != wantEntryFeed || m.Entry.Spec.InputCostPerM != wantIn):
			t.Errorf("%s: entry = %+v, want feed %s at %v", id, m.Entry, wantEntryFeed, wantIn)
		}
		switch {
		case wantEffortsFeed == "" && m.Efforts != nil:
			t.Errorf("%s: efforts answer (%+v), want none", id, m.Efforts)
		case wantEffortsFeed != "" && (m.Efforts == nil || m.Efforts.Feed != wantEffortsFeed || strings.Join(m.Efforts.Values, ",") != wantEfforts):
			t.Errorf("%s: efforts = %+v, want feed %s values %s", id, m.Efforts, wantEffortsFeed, wantEfforts)
		}
		var got []contributor
		for _, c := range m.Contributors {
			got = append(got, contributor{
				level: c.Scope + ":" + c.ScopeID, feed: c.Feed,
				entry: c.Spec != nil, efforts: len(c.Efforts) > 0, effortsValues: strings.Join(c.Efforts, ","),
				answersEntry: c.AnswersEntry, answersEfforts: c.AnswersEfforts,
			})
		}
		if len(got) != len(want) {
			t.Errorf("%s: contributors = %+v, want %+v", id, got, want)
			return
		}
		for k := range want {
			if got[k] != want[k] {
				t.Errorf("%s: contributor %d = %+v, want %+v", id, k, got[k], want[k])
			}
		}
	}
	check("glm-5.3", "pliage", 2.5, "", "", []contributor{
		{level: "platform:", feed: "pliage", entry: true},
		{level: "org:org1", feed: "pliage", entry: true, answersEntry: true},
	})
	check("scaleway/deepseek-v4.1-flash", "", 0, "pliage", "low,high,xhigh,max", []contributor{
		{level: "platform:", feed: "pliage", efforts: true, effortsValues: "low,high,xhigh,max", answersEfforts: true},
	})
	check("kimi-k2", "", 0, "teamf", "max", []contributor{
		{level: "platform:", feed: "pliage", efforts: true, effortsValues: "high"},
		{level: "team:team-pf", feed: "teamf", efforts: true, effortsValues: "max", answersEfforts: true},
	})
	check("unknown-x", "", 0, "", "", nil)

	// Without a model, every id a level states is reported, sorted.
	code, out = live.do(t, "GET", base, live.viewer, "")
	if code != http.StatusOK {
		t.Fatalf("the unfiltered view: %d %s", code, out)
	}
	view = priceFeedEffectiveWire{}
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	var ids []string
	for _, m := range view.Models {
		ids = append(ids, m.ID)
	}
	if got := strings.Join(ids, " "); got != "glm-5.3 kimi-k2 scaleway/deepseek-v4.1-flash" {
		t.Fatalf("the unfiltered view reports %q", got)
	}
}
