package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// priceFeedLive is the production New()/routes()/middleware boot with the
// price feed's memory stores: org1 holds team-pf, u-orgadmin administers
// org1, u-pfviewer views team-pf, u-outsider belongs to another team of
// org1 only, and root is the platform super-admin.
type priceFeedLive struct {
	s        *Server
	audit    audit.Store
	configs  *pricefeed.MemoryConfigStore
	states   *pricefeed.MemoryStateStore
	root     string
	orgAdmin string
	viewer   string
	outsider string
	do       func(t *testing.T, method, path, token, body string) (int, []byte)
}

func newPriceFeedLive(t *testing.T) *priceFeedLive {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	ids := identity.NewMemoryStore()
	svc, err := auth.NewService(auth.Config{
		Store:      ids,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	live := &priceFeedLive{
		audit:   audit.NewMemoryStore(),
		configs: pricefeed.NewMemoryConfigStore(),
		states:  pricefeed.NewMemoryStateStore(),
	}
	live.s = New(Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		Audit:                   live.audit,
		PriceFeedConfigs:        live.configs,
		PriceFeedStates:         live.states,
	}, iterlog.New(iterlog.LevelError, nil))

	ctx := context.Background()
	seedPlatformAdmin(t, ids, "root")
	for _, u := range []string{"u-orgadmin", "u-pfviewer", "u-outsider"} {
		if _, err := ids.CreateUser(ctx, identity.User{ID: u, Email: u + "@example.org", Status: identity.UserStatusActive, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("seed user %s: %v", u, err)
		}
	}
	if _, err := ids.CreateOrg(ctx, identity.Org{ID: "org1", Name: "org1", Slug: "org1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	for _, team := range []string{"team-pf", "team-other"} {
		if _, err := ids.CreateTeam(ctx, identity.Team{ID: team, Name: team, Slug: team, OrgID: "org1", CreatedAt: time.Now()}); err != nil {
			t.Fatalf("seed team %s: %v", team, err)
		}
	}
	if err := ids.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: "u-orgadmin", OrgID: "org1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("seed org admin: %v", err)
	}
	if err := ids.UpsertMembership(ctx, identity.Membership{UserID: "u-pfviewer", TeamID: "team-pf", Role: identity.RoleViewer, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	if err := ids.UpsertMembership(ctx, identity.Membership{UserID: "u-outsider", TeamID: "team-other", Role: identity.RoleAdmin, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("seed outsider: %v", err)
	}
	issue := func(id auth.Identity) string {
		t.Helper()
		tok, _, err := signer.IssueAccess(id)
		if err != nil {
			t.Fatalf("issue %s: %v", id.UserID, err)
		}
		return tok
	}
	live.root = issue(auth.Identity{UserID: "root", IsSuperAdmin: true, TeamID: "team-root"})
	live.orgAdmin = issue(auth.Identity{UserID: "u-orgadmin"})
	live.viewer = issue(auth.Identity{UserID: "u-pfviewer", TeamID: "team-pf", Role: identity.RoleViewer})
	live.outsider = issue(auth.Identity{UserID: "u-outsider", TeamID: "team-other", Role: identity.RoleAdmin})

	hs := httptest.NewServer(live.s.handler)
	t.Cleanup(hs.Close)
	live.do = func(t *testing.T, method, path, token, body string) (int, []byte) {
		t.Helper()
		return llmDo(t, hs, method, path, token, body)
	}
	return live
}

// declaredFeed is a declared feed carrying the given entries document.
func declaredFeed(name, entries string) pricefeed.FeedConfig {
	return pricefeed.FeedConfig{
		Name: name, Type: pricefeed.TypeDeclared, Enabled: true,
		Interval: pricefeed.D(30 * 24 * time.Hour), Timeout: pricefeed.D(time.Second),
		Params: map[string]string{"entries": entries},
	}
}

// pliageEntries is the declaration the platform scope exists for: a
// priced gateway id and an efforts-only one.
const pliageEntries = `{"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15},"scaleway/deepseek-v4.1-flash":{"reasoning_efforts":["low","high","xhigh","max"]}}`

// priceFeedsViewWire decodes the GET/PUT answer by its JSON names.
type priceFeedsViewWire struct {
	Scope     string     `json:"scope"`
	Origin    string     `json:"origin"`
	UpdatedAt *time.Time `json:"updated_at"`
	Feeds     []struct {
		Feed       pricefeed.FeedConfig `json:"feed"`
		Status     string               `json:"status"`
		Entries    int                  `json:"entries"`
		FreshFor   int64                `json:"fresh_for"`
		DeclaredAt *time.Time           `json:"declared_at"`
		LastError  string               `json:"last_error"`
	} `json:"feeds"`
}

// The platform scope's write surface: super-admin only (an org admin is
// refused on every verb), CAS on the stored stamp, the write audited in
// the PLATFORM log under its own action, and the manual sync.
func TestPlatformPriceFeedRoutes(t *testing.T) {
	live := newPriceFeedLive(t)
	const url = "/api/admin/price-feeds"
	body := priceFeedPutFeedBody(declaredFeed("pliage", pliageEntries))

	for _, verb := range []struct{ method, path, body string }{
		{"GET", url, ""},
		{"PUT", url, body},
		{"POST", url + "/sync", `{"name":"pliage"}`},
	} {
		if code, out := live.do(t, verb.method, verb.path, live.orgAdmin, verb.body); code != http.StatusForbidden {
			t.Fatalf("an org admin reached %s %s: %d %s", verb.method, verb.path, code, out)
		}
	}
	if rec, _ := live.configs.Get(context.Background(), "platform"); rec != nil {
		t.Fatalf("a refused write stored the platform document: %+v", rec)
	}

	code, out := live.do(t, "GET", url, live.root, "")
	if code != http.StatusOK {
		t.Fatalf("super-admin GET: %d %s", code, out)
	}
	var view priceFeedsViewWire
	if err := json.Unmarshal(out, &view); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if view.Scope != "platform" || view.Origin != "default" {
		t.Fatalf("the empty platform view is wrong: %+v", view)
	}

	code, out = live.do(t, "PUT", url, live.root, body)
	if code != http.StatusOK {
		t.Fatalf("super-admin PUT: %d %s", code, out)
	}
	if err := json.Unmarshal(out, &view); err != nil || view.UpdatedAt == nil {
		t.Fatalf("the PUT answered no stamp: %v (%s)", err, out)
	}
	rec, err := live.configs.Get(context.Background(), "platform")
	if err != nil || rec == nil || len(rec.Feeds) != 1 || rec.Feeds[0].Name != "pliage" {
		t.Fatalf("the platform document is not stored under %q: %+v err %v", "platform", rec, err)
	}

	// Audited in the platform log, by the super-admin, on the platform
	// document. The write is detached from the request: poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	for {
		events, err := live.audit.ListPlatform(context.Background(), audit.Page{Limit: 10})
		if err != nil {
			t.Fatalf("audit list: %v", err)
		}
		if len(events) > 0 {
			e := events[0]
			if e.Action != "platform.price_feeds.updated" || e.Target != "price_feeds" || e.TargetID != "platform" {
				t.Fatalf("audit row = %s %s/%s, want platform.price_feeds.updated price_feeds/platform", e.Action, e.Target, e.TargetID)
			}
			if e.ActorID != "root" || e.ActorKind != "super_admin" {
				t.Fatalf("audit actor = %q/%q", e.ActorID, e.ActorKind)
			}
			if n, ok := e.Meta["feeds"].(int); !ok || n != 1 {
				t.Fatalf("audit meta feeds = %v, want 1", e.Meta["feeds"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the platform write was never audited in the platform log")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A stale CAS loses as a 409; the current stamp wins.
	stale := `{"expected_updated_at":"` + view.UpdatedAt.Add(-time.Second).UTC().Format(time.RFC3339Nano) + `",` + strings.TrimPrefix(body, "{")
	if code, out := live.do(t, "PUT", url, live.root, stale); code != http.StatusConflict {
		t.Fatalf("a stale CAS did not lose: %d %s", code, out)
	}
	current := `{"expected_updated_at":"` + view.UpdatedAt.UTC().Format(time.RFC3339Nano) + `",` + strings.TrimPrefix(body, "{")
	if code, out := live.do(t, "PUT", url, live.root, current); code != http.StatusOK {
		t.Fatalf("the current CAS lost: %d %s", code, out)
	}

	// The manual sync reaches the platform document's feeds.
	code, out = live.do(t, "POST", url+"/sync", live.root, `{"name":"pliage"}`)
	if code != http.StatusOK {
		t.Fatalf("super-admin sync: %d %s", code, out)
	}
	var synced struct {
		Results []pricefeed.SyncOutcome `json:"results"`
	}
	if err := json.Unmarshal(out, &synced); err != nil || len(synced.Results) != 1 || synced.Results[0].Feed != "pliage" {
		t.Fatalf("the platform sync answered %s (err %v)", out, err)
	}
}

// The launch fold reads the platform document below the org and the team,
// for prices and efforts alike: per id the most specific scope stating the
// field answers, and a platform word reaches the ids no tenant states. The
// three documents are written in both orders — the precedence is the
// fold's, never the store's.
func TestEffectivePriceFeedFoldsThePlatformScope(t *testing.T) {
	const (
		platformDoc = `{"only-platform":{"input_usd_per_mtok":1,"output_usd_per_mtok":1},"platform-org":{"input_usd_per_mtok":1,"output_usd_per_mtok":1},"all":{"input_usd_per_mtok":1,"output_usd_per_mtok":1,"reasoning_efforts":["low","high"]},"efforts-only":{"reasoning_efforts":["high","max"]},"efforts-both":{"reasoning_efforts":["low"]}}`
		orgDoc      = `{"platform-org":{"input_usd_per_mtok":2,"output_usd_per_mtok":2},"all":{"input_usd_per_mtok":2,"output_usd_per_mtok":2}}`
		teamDoc     = `{"all":{"input_usd_per_mtok":3,"output_usd_per_mtok":3},"efforts-both":{"reasoning_efforts":["xhigh"]}}`
	)
	docs := []struct{ docID, feed, entries string }{
		{"platform", "plat", platformDoc},
		{pricefeed.OrgConfigDocID("org1"), "orgf", orgDoc},
		{pricefeed.TeamConfigDocID("team1"), "teamf", teamDoc},
	}
	for _, order := range []string{"platform first", "team first"} {
		t.Run(order, func(t *testing.T) {
			s, _ := priceFeedFixtureServer(t)
			writes := append([]struct{ docID, feed, entries string }(nil), docs...)
			if order == "team first" {
				writes[0], writes[2] = writes[2], writes[0]
			}
			for _, w := range writes {
				if err := s.priceFeedConfigs.Put(context.Background(), w.docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{declaredFeed(w.feed, w.entries)}}); err != nil {
					t.Fatal(err)
				}
			}
			raw := EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", nil, nil)
			table := pricefeed.ParseTable(string(raw))
			if table == nil {
				t.Fatalf("the fold answered no table: %s", raw)
			}
			for id, want := range map[string]struct {
				feed string
				in   float64
			}{
				"only-platform": {"plat", 1},
				"platform-org":  {"orgf", 2},
				"all":           {"teamf", 3},
			} {
				got, ok := table.Entries[id]
				if !ok || got.Feed != want.feed || got.Spec.InputCostPerM != want.in {
					t.Errorf("entry %s = %+v (present %v), want feed %s at %v", id, got, ok, want.feed, want.in)
				}
			}
			for id, want := range map[string]struct {
				feed   string
				values string
			}{
				// The platform states efforts the team's pricing never names.
				"all":          {"plat", "low,high"},
				"efforts-only": {"plat", "high,max"},
				"efforts-both": {"teamf", "xhigh"},
			} {
				got, ok := table.Efforts[id]
				if !ok || got.Feed != want.feed || strings.Join(got.Values, ",") != want.values {
					t.Errorf("efforts %s = %+v (present %v), want feed %s values %s", id, got, ok, want.feed, want.values)
				}
			}
		})
	}
}

// A declaration answers from its config: the view says `declared`, counts
// its entries and stamps it with the config's write — never a staleness
// derived from a state document, which a manual sync may have left under
// its name. A stored declaration the write rules refuse reads failing,
// naming the refusal: the fold steps it down.
func TestPriceFeedViewReportsADeclaration(t *testing.T) {
	s, _ := priceFeedFixtureServer(t)
	orgAdmin := asUser("u-orgadmin")
	const url = "/api/orgs/org1/price-feeds"

	w := httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, priceFeedPutFeedBody(declaredFeed("pliage", pliageEntries))))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT declared: %d %s", w.Code, w.Body.String())
	}
	var view priceFeedsViewWire
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Feeds) != 1 {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	got := view.Feeds[0]
	if got.Status != "declared" || got.Entries != 2 || got.DeclaredAt == nil || !got.DeclaredAt.Equal(*view.UpdatedAt) || got.FreshFor != 0 {
		t.Fatalf("the declaration's view = status %q entries %d declared_at %v fresh_for %d, want declared/2/%v/0", got.Status, got.Entries, got.DeclaredAt, got.FreshFor, view.UpdatedAt)
	}

	// A state document aged past any window under the declaration's name
	// (a manual sync's leftover) does not turn the declaration stale.
	if err := s.priceFeedStates.Put(context.Background(), pricefeed.StateDocID(pricefeed.OrgConfigDocID("org1"), "pliage"), &pricefeed.StateRecord{
		Feed: "pliage", Status: pricefeed.StateOK,
		Entries:     map[string]modelspecs.Spec{"glm-5.3": {InputCostPerM: 2.01, OutputCostPerM: 6.15}},
		RefreshedAt: time.Now().Add(-365 * 24 * time.Hour), FreshFor: pricefeed.D(time.Hour),
		LastAttempt: time.Now().Add(-365 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleGetOrgPriceFeeds(w, priceFeedReq(orgAdmin, "GET", url, ""))
	view = priceFeedsViewWire{}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Feeds) != 1 {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if view.Feeds[0].Status != "declared" {
		t.Fatalf("an aged state under the declaration's name made it %q", view.Feeds[0].Status)
	}

	// A stored declaration the write rules refuse: failing, the refusal named.
	broken := declaredFeed("pliage", `{"glm-5.3":{"context_window":256000}}`)
	if err := s.priceFeedConfigs.Put(context.Background(), pricefeed.OrgConfigDocID("org1"), &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{broken}}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleGetOrgPriceFeeds(w, priceFeedReq(orgAdmin, "GET", url, ""))
	view = priceFeedsViewWire{}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Feeds) != 1 {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if f := view.Feeds[0]; f.Status != pricefeed.StateFailing || !strings.Contains(f.LastError, "glm-5.3") || f.DeclaredAt != nil {
		t.Fatalf("a refused declaration reads status %q error %q declared_at %v, want failing naming glm-5.3", f.Status, f.LastError, f.DeclaredAt)
	}
}

// The reconciler never judges a declaration by a state document: an aged
// state under a declared feed's name (a manual sync's leftover) neither
// refreshes nor says a staleness transition — the fold reads the config.
func TestPriceFeedSweepLeavesADeclarationAlone(t *testing.T) {
	s, logs := priceFeedFixtureServer(t)
	docID := pricefeed.OrgConfigDocID("org1")
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{declaredFeed("pliage", pliageEntries)}}); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-365 * 24 * time.Hour)
	stateID := pricefeed.StateDocID(docID, "pliage")
	if err := s.priceFeedStates.Put(context.Background(), stateID, &pricefeed.StateRecord{
		Feed: "pliage", Status: pricefeed.StateOK,
		Entries:     map[string]modelspecs.Spec{"glm-5.3": {InputCostPerM: 2.01, OutputCostPerM: 6.15}},
		RefreshedAt: aged, FreshFor: pricefeed.D(time.Hour), LastAttempt: aged,
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.priceFeedStates.Get(context.Background(), stateID)
	logs.Reset()
	for range 2 {
		s.priceFeedSyncSweep(context.Background())
	}
	if strings.Contains(logs.String(), "pliage") {
		t.Fatalf("the sweep judged the declaration by its state: %q", logs.String())
	}
	after, _ := s.priceFeedStates.Get(context.Background(), stateID)
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("the sweep refreshed a declaration: %+v", after)
	}
}
