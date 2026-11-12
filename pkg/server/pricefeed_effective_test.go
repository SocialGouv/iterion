package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
// the one the fold took answering, so two feeds of one name at two levels
// are told apart. The fold's warnings and the runner's unseen fallback
// ride along.
func TestPriceFeedEffectiveView(t *testing.T) {
	live := newPriceFeedLive(t)
	// The platform and the org both name their feed "pliage": the answer
	// is told by the level the fold took it from, never by the name.
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
	sealed := mustTable(t)(PublisherPriceFeedEffective(live.configs, live.states, nil)(context.Background(), "org1", "team-pf",
		[]string{"glm-5.3", "Scaleway/DeepSeek-v4.1-flash", "kimi-k2", "unknown-x"}))
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

// bigDeclaration is a declared document over the run channel's byte
// budget: n ids model-0000…, each priced or stating efforts alone, beside
// scaleway/deepseek-v4.1-flash's accepted efforts.
func bigDeclaration(n int, priced bool) string {
	var b strings.Builder
	b.WriteString(`{"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}`)
	for i := range n {
		if priced {
			fmt.Fprintf(&b, `,"model-%04d":{"input_usd_per_mtok":1,"output_usd_per_mtok":2,"context_window":128000}`, i)
		} else {
			fmt.Fprintf(&b, `,"model-%04d":{"reasoning_efforts":["low","high"]}`, i)
		}
	}
	b.WriteString("}")
	return b.String()
}

// A listing (no ?model=) whose folded table exceeds the run channel's byte
// budget is a table no run seals whole: each run seals it narrowed to the
// gateway ids its workflow names. The view says so — over_budget names the
// size and the budget, the table is null, no line claims the operator
// table or "0 named ids" as the run's outcome — and every id answers as a
// run naming it reads it: the declaration's efforts, its prices. Asked
// for one id, the view answers the table a run naming it seals, the very
// bytes the publisher's fold seals for it. Two shapes: the efforts alone
// over budget, and the prices over budget beside efforts that fit.
func TestPriceFeedEffectiveViewListsAnOverBudgetTableAsEachRunNarrowsIt(t *testing.T) {
	const deepseek = "scaleway/deepseek-v4.1-flash"
	for _, tc := range []struct {
		name   string
		n      int
		priced bool
	}{
		{"efforts alone over budget", 3000, false},
		{"prices over budget", 1000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := newPriceFeedLive(t)
			putFeedDoc(t, live.configs, pricefeed.PlatformConfigDocID, declaredFeed("big", bigDeclaration(tc.n, tc.priced)))
			const base = "/api/teams/team-pf/price-feeds/effective"

			code, out := live.do(t, "GET", base, live.viewer, "")
			if code != http.StatusOK {
				t.Fatalf("the listing: %d %s", code, out)
			}
			var view struct {
				priceFeedEffectiveWire
				OverBudget *struct {
					Bytes  int `json:"bytes"`
					Budget int `json:"budget"`
				} `json:"over_budget"`
			}
			if err := json.Unmarshal(out, &view); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if view.OverBudget == nil || view.OverBudget.Budget != pricefeed.DefaultBudget || view.OverBudget.Bytes <= pricefeed.DefaultBudget {
				t.Errorf("over_budget = %+v, want the listing's size over the %d-byte budget", view.OverBudget, pricefeed.DefaultBudget)
			}
			if table := strings.TrimSpace(string(view.Table)); table != "" && table != "null" {
				t.Errorf("the listing shows a table no run seals whole: %.200s…", table)
			}
			warnings := strings.Join(view.Warnings, "\n")
			for _, claim := range []string{"the run keeps the operator table", "the run's 0 named"} {
				if strings.Contains(warnings, claim) {
					t.Errorf("a warning states %q as the run's outcome: %q", claim, view.Warnings)
				}
			}
			if !strings.Contains(warnings, "a run seals it narrowed to the gateway ids it names") {
				t.Errorf("no warning says how a run seals the table: %q", view.Warnings)
			}
			if len(view.Models) != tc.n+1 {
				t.Fatalf("the listing reports %d ids, want %d", len(view.Models), tc.n+1)
			}
			byID := map[string]int{}
			for i, m := range view.Models {
				byID[m.ID] = i
			}
			if m := view.Models[byID[deepseek]]; m.Efforts == nil || m.Efforts.Feed != "big" || strings.Join(m.Efforts.Values, ",") != "low,high,xhigh,max" {
				t.Errorf("%s answers efforts %+v, want the declaration's", deepseek, m.Efforts)
			}
			m := view.Models[byID["model-0007"]]
			if tc.priced && (m.Entry == nil || m.Entry.Feed != "big" || m.Entry.Spec.InputCostPerM != 1) {
				t.Errorf("model-0007 answers entry %+v, want the declaration's price", m.Entry)
			}
			if !tc.priced && (m.Efforts == nil || strings.Join(m.Efforts.Values, ",") != "low,high") {
				t.Errorf("model-0007 answers efforts %+v, want the declaration's", m.Efforts)
			}

			code, out = live.do(t, "GET", base+"?model="+deepseek, live.viewer, "")
			if code != http.StatusOK {
				t.Fatalf("the view for one id: %d %s", code, out)
			}
			view.OverBudget, view.Table = nil, nil
			if err := json.Unmarshal(out, &view); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if view.OverBudget != nil {
				t.Errorf("the view for one id flags over_budget %+v: its table is the run's", view.OverBudget)
			}
			sealed := mustTable(t)(PublisherPriceFeedEffective(live.configs, live.states, nil)(context.Background(), "org1", "team-pf", []string{deepseek}))
			var gotTable, wantTable bytes.Buffer
			if err := json.Compact(&gotTable, view.Table); err != nil {
				t.Fatalf("the view's table is not JSON: %v (%.200s)", err, view.Table)
			}
			if err := json.Compact(&wantTable, sealed); err != nil {
				t.Fatalf("the sealed table is not JSON: %v", err)
			}
			if gotTable.String() != wantTable.String() || !strings.Contains(gotTable.String(), `"`+deepseek+`":{"feed":"big"`) {
				t.Errorf("the view for one id answers %.300s…, want the sealed %.300s…", gotTable.String(), wantTable.String())
			}
		})
	}
}

// failingConfigStore answers one document's read with a database error and
// every other through the store it wraps.
type failingConfigStore struct {
	pricefeed.ConfigStore
	docID string
	err   error
}

func (f failingConfigStore) Get(ctx context.Context, docID string) (*pricefeed.ConfigRecord, error) {
	if docID == f.docID {
		return nil, f.err
	}
	return f.ConfigStore.Get(ctx, docID)
}

// A level the view cannot read is reported, and a member is told so without
// the database's words: the scope reads the member text, the warning names
// the level, says a run launched now is refused until it reads and points
// at the server log, and the server log carries the read error whole. The
// publisher's fold of the same levels refuses with the typed error naming
// the level and not the cause, and warns the operator's log with the cause.
// The readable level still answers in the view.
func TestPriceFeedEffectiveViewKeepsAReadErrorInTheServerLog(t *testing.T) {
	s, logs := priceFeedFixtureServer(t)
	const internal = "server selection error: context deadline exceeded, current topology: { Type: ReplicaSetNoPrimary, Servers: [{ Addr: mongo-0.mongo.internal:27017 }] }"
	mem := s.priceFeedConfigs
	putFeedDoc(t, mem, pricefeed.TeamConfigDocID("team1"), declaredFeed("teamf", `{"glm-5.3":{"input_usd_per_mtok":3,"output_usd_per_mtok":9}}`))
	s.priceFeedConfigs = failingConfigStore{ConfigStore: mem, docID: pricefeed.PlatformConfigDocID, err: errors.New(internal)}

	w := httptest.NewRecorder()
	s.handleGetTeamPriceFeedsEffective(w, priceFeedReq(asUser("u-teamviewer"), "GET", "/api/teams/team1/price-feeds/effective", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("the view over an unread level: %d %s", w.Code, w.Body.String())
	}
	var view priceFeedEffectiveWire
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(view.Scopes) == 0 || view.Scopes[0].Scope != "platform" || view.Scopes[0].Unread != priceFeedUnreadForMembers {
		t.Errorf("the unread platform level reads %+v, want unread %q", view.Scopes, priceFeedUnreadForMembers)
	}
	var unreadLine string
	for _, line := range view.Warnings {
		if strings.Contains(line, "platform unread") {
			unreadLine = line
		}
	}
	if unreadLine == "" || strings.Contains(unreadLine, "mongo") || strings.Contains(unreadLine, "server selection") || !strings.Contains(unreadLine, priceFeedUnreadForMembers) ||
		!strings.Contains(unreadLine, "a run launched now is refused until it reads") {
		t.Errorf("the warning on the unread level = %q (warnings %q), want it named, saying a launch is refused, and pointing at the server log, without the read error", unreadLine, view.Warnings)
	}
	if !strings.Contains(logs.String(), "mongo-0.mongo.internal:27017") || !strings.Contains(logs.String(), "team team1") {
		t.Errorf("the server log does not carry the read error for team team1's view:\n%s", logs.String())
	}
	if len(view.Models) != 1 || view.Models[0].Entry == nil || view.Models[0].Entry.Feed != "teamf" {
		t.Errorf("the readable team level does not answer: %+v", view.Models)
	}

	var operator []string
	raw, err := PublisherPriceFeedEffective(s.priceFeedConfigs, s.priceFeedStates, func(format string, args ...any) {
		operator = append(operator, fmt.Sprintf(format, args...))
	})(context.Background(), "", "team1", nil)
	var unread *PriceFeedUnreadError
	if raw != nil || !errors.As(err, &unread) || !slices.Equal(unread.Levels, []string{"platform"}) || !unread.Retryable() || unread.Code() != PriceFeedUnreadErrorCode {
		t.Fatalf("the publisher's fold answered %s, %v; want no table and a retryable *PriceFeedUnreadError naming the platform", raw, err)
	}
	if strings.Contains(err.Error(), "mongo") || strings.Contains(err.Error(), "server selection") {
		t.Errorf("the refusal %q carries the read error, which names internal topology", err)
	}
	if !strings.Contains(strings.Join(operator, "\n"), "platform unread — the launch or resume folding it is refused: "+internal) {
		t.Errorf("the publisher's fold warns %q, want the platform level's read error", operator)
	}
}

// One contributor answers each of an id's entry and efforts: the one the
// fold took, at the level it took it from. Two levels naming their feed
// alike and stating the same word are two contributors, and only the more
// specific one answers — the run reads the team's, whatever the platform
// says. Two shapes: a declaration's efforts, and a fetched feed's entry
// refreshed at the same instant at both levels. A later feed of the
// answering level stating the id answers neither.
func TestPriceFeedEffectiveViewNamesTheLevelThatAnswers(t *testing.T) {
	const deepseek = "scaleway/deepseek-v4.1-flash"
	live := newPriceFeedLive(t)
	efforts := `{"` + deepseek + `":{"reasoning_efforts":["low","high","xhigh","max"]}}`
	refreshed := time.Now().Add(-time.Minute).UTC()
	for _, docID := range []string{pricefeed.PlatformConfigDocID, pricefeed.TeamConfigDocID("team-pf")} {
		feeds := []pricefeed.FeedConfig{
			declaredFeed("pliage", efforts),
			{Name: "scw", Type: pricefeed.TypeExec, Enabled: true, Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(time.Second), Params: map[string]string{"command": "/nonexistent"}},
		}
		if docID != pricefeed.PlatformConfigDocID {
			feeds = append(feeds, declaredFeed("extra", `{"`+deepseek+`":{"reasoning_efforts":["high"]},"glm-5.3":{"input_usd_per_mtok":3,"output_usd_per_mtok":9}}`))
		}
		putFeedDoc(t, live.configs, docID, feeds...)
		if err := live.states.Put(context.Background(), pricefeed.StateDocID(docID, "scw"), &pricefeed.StateRecord{
			Feed: "scw", Type: pricefeed.TypeExec, Status: pricefeed.StateOK,
			Entries:     map[string]modelspecs.Spec{"glm-5.3": {ContextWindow: 256000, InputCostPerM: 2.01, OutputCostPerM: 6.15}},
			RefreshedAt: refreshed, FreshFor: pricefeed.D(2 * time.Hour), LastAttempt: refreshed,
		}); err != nil {
			t.Fatal(err)
		}
	}
	code, out := live.do(t, "GET", "/api/teams/team-pf/price-feeds/effective?model="+deepseek+"&model=glm-5.3", live.viewer, "")
	if code != http.StatusOK {
		t.Fatalf("the view: %d %s", code, out)
	}
	var view priceFeedEffectiveWire
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if len(view.Models) != 2 {
		t.Fatalf("the view reports %d ids, want 2: %s", len(view.Models), out)
	}
	for _, m := range view.Models {
		var answers []string
		for _, c := range m.Contributors {
			if c.AnswersEntry {
				answers = append(answers, "entry:"+c.Scope+":"+c.Feed)
			}
			if c.AnswersEfforts {
				answers = append(answers, "efforts:"+c.Scope+":"+c.Feed)
			}
		}
		want := map[string]string{deepseek: "efforts:team:pliage", "glm-5.3": "entry:team:scw"}[m.ID]
		if got := strings.Join(answers, " "); got != want {
			t.Errorf("%s: the contributors that answer are %q, want only %q — the feed and the level the fold took", m.ID, got, want)
		}
		if len(m.Contributors) != 3 {
			t.Errorf("%s: %d contributors, want the platform's, the team's like-named one and the team's later one", m.ID, len(m.Contributors))
		}
	}
}
