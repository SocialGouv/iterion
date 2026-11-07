package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// asSuperAdmin carries the fixture's super-admin identity.
func asSuperAdmin() context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "admin", IsSuperAdmin: true})
}

// priceFeedFixtureServer builds a server with the memory stores wired and
// the org/team world seeded, plus an exec-source script whose table the
// test rewrites between syncs.
func priceFeedFixtureServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	// The exec hatch (the fixture refreshes exec feeds) and the LLM
	// private-endpoint hatch (the litellm stubs serve on loopback — the
	// guard refusing them is the behaviour under test elsewhere).
	t.Setenv(priceFeedExecEnv, "1")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	s := newOrgTestServer(t)
	var logs bytes.Buffer
	s.logger = iterlog.New(iterlog.LevelWarn, &logs)
	s.priceFeedConfigs = pricefeed.NewMemoryConfigStore()
	s.priceFeedStates = pricefeed.NewMemoryStateStore()
	s.priceFeedTransitions = map[string]string{}
	seedRoutingPolicyWorld(t, s)
	return s, &logs
}

// writeTariffScript writes an exec source answering the given table.
func writeTariffScript(t *testing.T, in, out float64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tariffs.sh")
	body := "#!/bin/sh\nprintf '%s' '{\"glm-5.3\":{\"context_window\":131072,\"input_usd_per_mtok\":IN,\"output_usd_per_mtok\":OUT}}'\n"
	body = strings.ReplaceAll(body, "IN", formatPrice(in))
	body = strings.ReplaceAll(body, "OUT", formatPrice(out))
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func formatPrice(v float64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func priceFeedFeed(script string) pricefeed.FeedConfig {
	return pricefeed.FeedConfig{
		Name: "scw", Type: pricefeed.TypeExec, Enabled: true,
		Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(10 * time.Second),
		Params: map[string]string{"command": script},
	}
}

// liteLLMStub serves one tariff table in the litellm model_info shape and
// answers the feed config the tenant-level CRUD uses.
func liteLLMStub(t *testing.T, in, out float64) pricefeed.FeedConfig {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"model_name":"glm-5.3","model_info":{"context_window":131072,"input_cost_per_token":%ve-6,"output_cost_per_token":%ve-6,"max_tokens":131072}}]}`, in, out)
	}))
	t.Cleanup(srv.Close)
	return pricefeed.FeedConfig{
		Name: "scw", Type: pricefeed.TypeLiteLLM, Enabled: true,
		Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(10 * time.Second),
		Params: map[string]string{"url": srv.URL, "token": "tenant-litellm-bearer"},
	}
}

func priceFeedPutBody(script string) string {
	return priceFeedPutFeedBody(priceFeedFeed(script))
}

func priceFeedPutFeedBody(feed pricefeed.FeedConfig) string {
	raw, _ := json.Marshal([]pricefeed.FeedConfig{feed})
	return `{"feeds":` + string(raw) + `}`
}

func priceFeedReq(ictx context.Context, method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ictx)
	r.SetPathValue("id", strings.Split(path, "/")[3])
	return r
}

// The tenant surface: empty view, validated PUT, CAS 409, null-clear,
// RBAC, and the manual sync that reports what changed per id.
func TestPriceFeedRoutes(t *testing.T) {
	s, logs := priceFeedFixtureServer(t)
	orgAdmin := asUser("u-orgadmin")
	orgViewer := asUser("u-orgmember")
	const url = "/api/orgs/org1/price-feeds"

	// The empty view: origin default, the registered source vocabulary.
	w := httptest.NewRecorder()
	s.handleGetOrgPriceFeeds(w, priceFeedReq(orgAdmin, "GET", url, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("GET empty: %d", w.Code)
	}
	var empty struct {
		Origin      string   `json:"origin"`
		SourceTypes []string `json:"source_types"`
	}
	json.NewDecoder(w.Body).Decode(&empty)
	if empty.Origin != "default" || len(empty.SourceTypes) == 0 {
		t.Fatalf("the empty view is wrong: %+v", empty)
	}

	// A viewer reads, cannot write.
	w = httptest.NewRecorder()
	s.handleGetOrgPriceFeeds(w, priceFeedReq(orgViewer, "GET", url, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("a member cannot read: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgViewer, "PUT", url, priceFeedPutBody(writeTariffScript(t, 0.6, 1.8))))
	if w.Code != http.StatusForbidden {
		t.Fatalf("a member wrote the feed list: %d", w.Code)
	}

	// Validation: an unknown source type is a 400 naming the feed.
	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, `{"feeds":[{"name":"x","type":"nope","enabled":true,"interval":60,"timeout":10}]}`))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "nope") {
		t.Fatalf("an unknown source type accepted: %d %s", w.Code, w.Body.String())
	}

	// The exec gate: an org admin cannot write an exec feed (it runs on
	// the server); a super-admin can. The tenant-level CRUD below uses a
	// litellm feed — the shape a tenant admin legitimately owns.
	script := writeTariffScript(t, 0.6, 1.8)
	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, priceFeedPutBody(script)))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), priceFeedExecEnv) {
		t.Fatalf("an org admin wrote an exec feed: %d %s", w.Code, w.Body.String())
	}
	// The super-admin path: seed the fixture super-admin into the org
	// (canManageOrg gates first), then the exec feed stores.
	if err := s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: "admin", OrgID: "org1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed the super-admin's org membership: %v", err)
	}
	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(asSuperAdmin(), "PUT", url, priceFeedPutBody(script)))
	if w.Code != http.StatusOK {
		t.Fatalf("super-admin PUT exec: %d %s", w.Code, w.Body.String())
	}
	// Drop it again: the tenant CRUD proceeds on a litellm feed.
	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(asUser("admin"), "PUT", url, `{"feeds":null}`))
	if w.Code != http.StatusOK {
		t.Fatalf("clear exec: %d", w.Code)
	}

	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, priceFeedPutFeedBody(liteLLMStub(t, 0.6, 1.8))))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT valid: %d %s", w.Code, w.Body.String())
	}
	var stored struct {
		UpdatedAt *time.Time `json:"updated_at"`
	}
	json.NewDecoder(w.Body).Decode(&stored)
	if stored.UpdatedAt == nil {
		t.Fatal("the CAS stamp did not come back")
	}

	// A stale CAS loses as a 409.
	w = httptest.NewRecorder()
	stale := *stored.UpdatedAt
	staleBody := strings.Replace(priceFeedPutFeedBody(liteLLMStub(t, 0.6, 1.8)), "{", `{"expected_updated_at":"`+stale.Add(-time.Second).UTC().Format(time.RFC3339Nano)+`",`, 1)
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, staleBody))
	if w.Code != http.StatusConflict {
		t.Fatalf("a stale CAS did not lose: %d", w.Code)
	}
	// A current CAS wins.
	w = httptest.NewRecorder()
	freshBody := strings.Replace(priceFeedPutFeedBody(liteLLMStub(t, 0.6, 1.8)), "{", `{"expected_updated_at":"`+stored.UpdatedAt.UTC().Format(time.RFC3339Nano)+`",`, 1)
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, freshBody))
	if w.Code != http.StatusOK {
		t.Fatalf("a current CAS lost: %d", w.Code)
	}

	// The view redacts token-shaped params for every reader (the feed
	// list is member-readable; the litellm bearer is not).
	w = httptest.NewRecorder()
	s.handleGetOrgPriceFeeds(w, priceFeedReq(orgViewer, "GET", url, ""))
	if strings.Contains(w.Body.String(), "tenant-litellm-bearer") {
		t.Fatalf("the view leaked the litellm bearer to a member: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "(redacted)") {
		t.Fatalf("the view does not mark the redaction: %s", w.Body.String())
	}

	// The manual sync: first refresh reports every entry as new, the
	// second as unchanged, and a rewritten tariff as old → new.
	w = httptest.NewRecorder()
	s.handleSyncOrgPriceFeeds(w, priceFeedReq(orgAdmin, "POST", url+"/sync", "{}"))
	if w.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", w.Code, w.Body.String())
	}
	var first struct {
		Results []pricefeed.SyncOutcome `json:"results"`
	}
	json.NewDecoder(w.Body).Decode(&first)
	if len(first.Results) != 1 || first.Results[0].Status != pricefeed.StateOK || len(first.Results[0].Changes) != 1 {
		t.Fatalf("the first sync's outcome is wrong: %+v", first.Results)
	}
	if ch := first.Results[0].Changes[0]; ch.ID != "glm-5.3" || ch.Old != nil || ch.New == nil || ch.New.InputCostPerM != 0.6 {
		t.Fatalf("the first sync's change is wrong: %+v", ch)
	}

	w = httptest.NewRecorder()
	s.handleSyncOrgPriceFeeds(w, priceFeedReq(orgAdmin, "POST", url+"/sync", "{}"))
	if w.Code != http.StatusOK {
		t.Fatalf("the second sync: %d %s", w.Code, w.Body.String())
	}
	first.Results = nil
	json.NewDecoder(w.Body).Decode(&first)
	if len(first.Results) != 1 || len(first.Results[0].Changes) != 0 {
		t.Fatalf("an unchanged table reported changes: %+v", first.Results)
	}

	// A rewritten tariff: the operator's change reaches the report.
	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, priceFeedPutFeedBody(liteLLMStub(t, 0.42, 1.4))))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT rewrite: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleSyncOrgPriceFeeds(w, priceFeedReq(orgAdmin, "POST", url+"/sync", `{"name":"scw"}`))
	var moved struct {
		Results []pricefeed.SyncOutcome `json:"results"`
	}
	json.NewDecoder(w.Body).Decode(&moved)
	if len(moved.Results) != 1 || len(moved.Results[0].Changes) != 1 {
		t.Fatalf("a named sync did not report the move: %+v", moved.Results)
	}
	ch := moved.Results[0].Changes[0]
	if ch.Old == nil || ch.New == nil || ch.New.InputCostPerM != 0.42 || ch.Old.InputCostPerM != 0.6 {
		t.Fatalf("the move's old → new is wrong: %+v", ch)
	}

	// Drift: the operator table and the feed disagree — the sync says so.
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"glm-5.3":{"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	w = httptest.NewRecorder()
	s.handleSyncOrgPriceFeeds(w, priceFeedReq(orgAdmin, "POST", url+"/sync", "{}"))
	if !strings.Contains(logs.String(), "glm-5.3") || !strings.Contains(logs.String(), "operator table") {
		t.Fatalf("the drift warning did not name the id and both sides: %q", logs.String())
	}

	// An unknown feed name is a 404; the null-clear empties the level.
	w = httptest.NewRecorder()
	s.handleSyncOrgPriceFeeds(w, priceFeedReq(orgAdmin, "POST", url+"/sync", `{"name":"nope"}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown feed name synced: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handlePutOrgPriceFeeds(w, priceFeedReq(orgAdmin, "PUT", url, `{"feeds":null}`))
	if w.Code != http.StatusOK {
		t.Fatalf("the null-clear: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleSyncOrgPriceFeeds(w, priceFeedReq(orgAdmin, "POST", url+"/sync", "{}"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("a cleared level still syncs: %d", w.Code)
	}
}

// The elected sweep: due feeds refresh, fresh ones are left alone, the
// staleness transition says itself once, and a disabled feed is never
// touched.
func TestPriceFeedSyncSweep(t *testing.T) {
	s, logs := priceFeedFixtureServer(t)
	script := writeTariffScript(t, 0.6, 1.8)
	docID := pricefeed.OrgConfigDocID("org1")
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{
		priceFeedFeed(script),
		{Name: "off", Type: pricefeed.TypeExec, Enabled: false, Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(time.Second), Params: map[string]string{"command": "/nonexistent"}},
	}}); err != nil {
		t.Fatal(err)
	}

	// First pass: the enabled feed is due and refreshes; the disabled one
	// is never fetched (its command does not even exist).
	s.priceFeedSyncSweep(context.Background())
	state, err := s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(docID, "scw"))
	if err != nil || state == nil || state.Status != pricefeed.StateOK || state.Entries["glm-5.3"].InputCostPerM != 0.6 {
		t.Fatalf("the sweep did not refresh the due feed: %+v err %v", state, err)
	}
	if st, _ := s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(docID, "off")); st != nil {
		t.Fatal("the disabled feed was touched")
	}

	// A feed refreshed a second ago is not due: rewriting the source must
	// not reach the state on the very next sweep.
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{priceFeedFeed(writeTariffScript(t, 9, 9))}}); err != nil {
		t.Fatal(err)
	}
	s.priceFeedSyncSweep(context.Background())
	state, _ = s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(docID, "scw"))
	if state.Entries["glm-5.3"].InputCostPerM != 0.6 {
		t.Fatalf("a fresh feed was re-fetched: %+v", state.Entries)
	}

	// Age the state past its window: the derived status is stale, the
	// transition says itself ONCE across passes.
	state.RefreshedAt = time.Now().Add(-3 * time.Hour)
	if err := s.priceFeedStates.Put(context.Background(), pricefeed.StateDocID(docID, "scw"), state); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	for range 2 {
		s.priceFeedSyncSweep(context.Background())
	}
	if got := strings.Count(logs.String(), "stepped down"); got != 1 {
		t.Fatalf("the staleness transition warned %d times across two passes, want 1: %q", got, logs.String())
	}
	// The backoff makes the stale feed due again: the sweep re-fetches,
	// and the fresh state clears the transition memo's subject.
	script2 := writeTariffScript(t, 0.42, 1.4)
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{priceFeedFeed(script2)}}); err != nil {
		t.Fatal(err)
	}
	// The backoff makes a FAILING feed due after its short retry, not
	// the full interval: age the attempt with the error kept.
	state.LastError = "aged out"
	state.LastAttempt = time.Now().Add(-2 * time.Minute)
	if err := s.priceFeedStates.Put(context.Background(), pricefeed.StateDocID(docID, "scw"), state); err != nil {
		t.Fatal(err)
	}
	s.priceFeedSyncSweep(context.Background())
	state, _ = s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(docID, "scw"))
	if state.Status != pricefeed.StateOK || state.Entries["glm-5.3"].InputCostPerM != 0.42 {
		t.Fatalf("the backoff did not make the failing feed due: %+v", state)
	}
}

// The launch-path fold: org and team fold with team answering over org
// per id, nil stores answer nil, and no fresh feed answers nil.
func TestEffectivePriceFeedFold(t *testing.T) {
	s, _ := priceFeedFixtureServer(t)
	spec := func(in float64) modelspecs.Spec { return modelspecs.Spec{InputCostPerM: in, OutputCostPerM: in} }
	feedCfg := func(name string) pricefeed.FeedConfig {
		return pricefeed.FeedConfig{Name: name, Type: pricefeed.TypeExec, Enabled: true,
			Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(time.Second)}
	}
	put := func(docID, feed string, in float64) {
		t.Helper()
		if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{feedCfg(feed)}}); err != nil {
			t.Fatal(err)
		}
		if err := s.priceFeedStates.Put(context.Background(), pricefeed.StateDocID(docID, feed), &pricefeed.StateRecord{
			Feed: feed, Entries: map[string]modelspecs.Spec{"glm-5.3": spec(in), "team-only": spec(in)},
			RefreshedAt: time.Now(), FreshFor: pricefeed.D(2 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := EffectivePriceFeed(context.Background(), nil, nil, "org1", "team1", nil, nil); got != nil {
		t.Fatal("nil stores folded a table")
	}
	if got := EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", nil, nil); got != nil {
		t.Fatal("an empty world folded a table")
	}
	put(pricefeed.OrgConfigDocID("org1"), "org-feed", 1)
	raw := EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", nil, nil)
	table := pricefeed.ParseTable(string(raw))
	if table == nil || table.Entries["glm-5.3"].Spec.InputCostPerM != 1 {
		t.Fatalf("the org feed did not fold: %s", raw)
	}
	put(pricefeed.TeamConfigDocID("team1"), "team-feed", 2)
	raw = EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", nil, nil)
	table = pricefeed.ParseTable(string(raw))
	if table.Entries["glm-5.3"].Spec.InputCostPerM != 2 || table.Entries["glm-5.3"].Feed != "team-feed" {
		t.Fatalf("the team feed did not answer over the org: %s", raw)
	}
	if table.Entries["team-only"].Spec.InputCostPerM != 2 {
		t.Fatalf("a team-only id did not fold: %s", raw)
	}
}

// Round 1's CRITICAL, witnessed twice: the tenant write surface refuses
// an exec feed (super-admin only), and the ONE execution body refuses to
// run one unless the deployment operator armed the hatch — so a document
// no route should have been able to write still does not execute.
func TestPriceFeedExecGates(t *testing.T) {
	s, _ := priceFeedFixtureServer(t)

	// The execution-side hatch, read WITHOUT the fixture's arming: build
	// a server whose process has the hatch unset. t.Setenv in the fixture
	// is process-wide, so this test toggles it explicitly (serial file).
	docID := pricefeed.TeamConfigDocID("team1")
	feed := priceFeedFeed(writeTariffScript(t, 0.6, 1.8))
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{feed}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(priceFeedExecEnv, "")
	outcome, err := s.priceFeedRefresh(context.Background(), docID, feed)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != pricefeed.StateFailing || !strings.Contains(outcome.Error, priceFeedExecEnv) {
		t.Fatalf("an exec feed executed without the hatch: %+v", outcome)
	}
	state, _ := s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(docID, feed.Name))
	if state == nil || state.Status != pricefeed.StateFailing || !strings.Contains(state.LastError, priceFeedExecEnv) {
		t.Fatalf("the refusal did not reach the durable state: %+v", state)
	}
	// Arming the hatch lets it run.
	t.Setenv(priceFeedExecEnv, "1")
	outcome, err = s.priceFeedRefresh(context.Background(), docID, feed)
	if err != nil || outcome.Status != pricefeed.StateOK {
		t.Fatalf("an armed exec feed did not run: %+v err %v", outcome, err)
	}
}

// The staleness transition warns on EVERY fall, not once per process:
// the memo clears when the feed recovers (round 1's LOW: a silent
// step-down after the first cycle).
func TestPriceFeedTransitionRewarnsAfterRecovery(t *testing.T) {
	s, logs := priceFeedFixtureServer(t)
	docID := pricefeed.OrgConfigDocID("org1")
	feed := priceFeedFeed(writeTariffScript(t, 0.6, 1.8))
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{feed}}); err != nil {
		t.Fatal(err)
	}
	s.priceFeedSyncSweep(context.Background())
	// stamp rewrites the state: fresh or aged, and due (the attempt aged
	// past the interval) so the sweep's refresh actually fires.
	stamp := func(fresh bool) {
		t.Helper()
		state, _ := s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(docID, feed.Name))
		if state == nil {
			t.Fatal("no state to age")
		}
		if fresh {
			state.RefreshedAt = time.Now()
			state.LastError = ""
		} else {
			state.RefreshedAt = time.Now().Add(-3 * time.Hour)
		}
		state.LastAttempt = time.Now().Add(-2 * time.Hour)
		if err := s.priceFeedStates.Put(context.Background(), pricefeed.StateDocID(docID, feed.Name), state); err != nil {
			t.Fatal(err)
		}
	}
	stamp(false)
	logs.Reset()
	s.priceFeedSyncSweep(context.Background())
	if first := strings.Count(logs.String(), "stepped down"); first != 1 {
		t.Fatalf("the first fall warned %d times: %q", first, logs.String())
	}
	// Recover: the due sweep refreshes the aged state (fetch succeeds),
	// and the NEXT pass sees a healthy feed and clears the memo.
	s.priceFeedSyncSweep(context.Background())
	s.priceFeedSyncSweep(context.Background())
	stamp(false)
	logs.Reset()
	s.priceFeedSyncSweep(context.Background())
	if again := strings.Count(logs.String(), "stepped down"); again != 1 {
		t.Fatalf("the second fall warned %d times — the memo never cleared: %q", again, logs.String())
	}
}

// The gate's finding, witnessed twice: credential-bearing URLs never
// reach a member's view (userinfo and query stripped), and a late
// failing refresh does not revert a newer successful one.
func TestPriceFeedURLRedactionAndLateFailure(t *testing.T) {
	s, _ := priceFeedFixtureServer(t)
	// A litellm feed whose URL carries its token in the query.
	feed := liteLLMStub(t, 0.6, 1.8)
	feed.Params["url"] = feed.Params["url"] + "/info?token=SEKRIT&team=a"
	docID := pricefeed.TeamConfigDocID("team1")
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{feed}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleGetTeamPriceFeeds(w, priceFeedReq(asUser("u-teamadmin"), "GET", "/api/teams/team1/price-feeds", ""))
	if strings.Contains(w.Body.String(), "SEKRIT") {
		t.Fatalf("the view leaked the URL's token: %s", w.Body.String())
	}

	// The late-failing revert: a successful refresh lands, then an
	// overlapping refresh that read the OLDER state fails — its write is
	// refused, the tariffs stay the successful ones.
	outcome, err := s.priceFeedRefresh(context.Background(), docID, feed)
	if err != nil || outcome.Status != pricefeed.StateOK {
		t.Fatalf("the base refresh failed: %+v err %v", outcome, err)
	}
	failing := pricefeed.FeedConfig{Name: feed.Name, Type: pricefeed.TypeExec, Enabled: true,
		Interval: feed.Interval, Timeout: feed.Timeout, Params: map[string]string{"command": "/nonexistent"}}
	lateOutcome, err := s.priceFeedRefresh(context.Background(), docID, failing)
	if err != nil {
		t.Fatal(err)
	}
	if lateOutcome.Status != pricefeed.StateFailing {
		t.Fatalf("the late refresh did not fail: %+v", lateOutcome)
	}
	state, _ := s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(docID, feed.Name))
	if state.Status != pricefeed.StateOK || state.Entries["glm-5.3"].InputCostPerM != 0.6 {
		t.Fatalf("the late failing refresh reverted the successful state: %+v", state)
	}
}
