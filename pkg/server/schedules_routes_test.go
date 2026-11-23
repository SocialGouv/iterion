package server

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newScheduleTestServer wires an in-memory ScheduledBots store into a stock
// org test server and pins scheduleClock so NextFireAt is deterministic.
func newScheduleTestServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	s.cfg.ScheduledBots = cloudsched.NewMemoryStore()
	s.scheduleClock = func() time.Time { return time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC) }
	return s
}

func scheduleReq(ctx context.Context, method, path, body, teamID, sid string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	if teamID != "" {
		r.SetPathValue("id", teamID)
	}
	if sid != "" {
		r.SetPathValue("sid", sid)
	}
	return r
}

func TestSchedule_CreateHappy(t *testing.T) {
	s := newScheduleTestServer(t)
	ctx := superAdminCtx()
	body := `{
		"bot_id":"feed-watch",
		"cron":"*/5 * * * *",
		"vars":{"mode":"digest","category":"go"},
		"repo_url":"https://example.com/repo.git",
		"repo_ref":"main"
	}`
	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t1/schedules", body, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created cloudsched.ScheduledBot
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.BotID != "feed-watch" || created.TenantID != "t1" {
		t.Fatalf("bad payload: %+v", created)
	}
	if created.RepoURL != "https://example.com/repo.git" || created.RepoRef != "main" {
		t.Errorf("repo fields dropped: %+v", created)
	}
	// NextFireAt must be > now and land on the next 5-minute slot for our
	// pinned scheduleClock (10:00:00 → 10:05:00).
	want := time.Date(2026, 6, 22, 10, 5, 0, 0, time.UTC)
	if !created.NextFireAt.Equal(want) {
		t.Errorf("NextFireAt: got %s want %s", created.NextFireAt, want)
	}
	if created.ID == "" || created.CreatedAt.IsZero() {
		t.Errorf("id/created_at not stamped: %+v", created)
	}
}

func TestSchedule_CreateBadCron(t *testing.T) {
	s := newScheduleTestServer(t)
	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(superAdminCtx(), "POST", "/api/teams/t1/schedules",
		`{"bot_id":"feed-watch","cron":"not a cron"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad cron: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSchedule_CreateMissingFields(t *testing.T) {
	s := newScheduleTestServer(t)
	w := httptest.NewRecorder()
	// no bot_id
	s.handleCreateSchedule(w, scheduleReq(superAdminCtx(), "POST", "/api/teams/t1/schedules",
		`{"cron":"*/5 * * * *"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing bot_id: code=%d", w.Code)
	}
}

func TestSchedule_ListAndCrossTeamGate(t *testing.T) {
	s := newScheduleTestServer(t)
	ctx := superAdminCtx()

	// Seed one row on t1 + one on t2, then list t1: only the t1 row must return.
	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t1/schedules",
		`{"bot_id":"a","cron":"0 * * * *"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("seed t1: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t2/schedules",
		`{"bot_id":"b","cron":"0 * * * *"}`, "t2", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("seed t2: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleListSchedules(w, scheduleReq(ctx, "GET", "/api/teams/t1/schedules", "", "t1", ""))
	var lr struct {
		Schedules []cloudsched.ScheduledBot `json:"schedules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
		t.Fatal(err)
	}
	if len(lr.Schedules) != 1 || lr.Schedules[0].BotID != "a" {
		t.Fatalf("list t1: %+v", lr.Schedules)
	}
}

func TestSchedule_UpdateCrossTeamIs404(t *testing.T) {
	s := newScheduleTestServer(t)
	ctx := superAdminCtx()

	// Seed a schedule on t1 …
	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t1/schedules",
		`{"bot_id":"a","cron":"0 * * * *","vars":{"mode":"digest"}}`, "t1", ""))
	var created cloudsched.ScheduledBot
	json.Unmarshal(w.Body.Bytes(), &created)

	// … then try to update it under team t2 → 404, not 200 or 403.
	w = httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(ctx, "PATCH",
		"/api/teams/t2/schedules/"+created.ID,
		`{"disabled":true}`, "t2", created.ID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-team update should 404: code=%d body=%s", w.Code, w.Body.String())
	}

	// Legitimate update from t1 → 200 and the disabled flag flips.
	w = httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(ctx, "PATCH",
		"/api/teams/t1/schedules/"+created.ID,
		`{"disabled":true,"cron":"*/10 * * * *"}`, "t1", created.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("legit update: code=%d body=%s", w.Code, w.Body.String())
	}
	var updated cloudsched.ScheduledBot
	json.Unmarshal(w.Body.Bytes(), &updated)
	if !updated.Disabled || updated.Cron != "*/10 * * * *" {
		t.Errorf("update payload: %+v", updated)
	}
	wantNext := time.Date(2026, 6, 22, 10, 10, 0, 0, time.UTC)
	if !updated.NextFireAt.Equal(wantNext) {
		t.Errorf("NextFireAt should follow new cron: got %s want %s", updated.NextFireAt, wantNext)
	}
	// Vars survive an update that didn't specify them (nil pointer = untouched).
	if updated.Vars["mode"] != "digest" {
		t.Errorf("vars dropped on partial update: %+v", updated.Vars)
	}
}

func TestSchedule_Delete(t *testing.T) {
	s := newScheduleTestServer(t)
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t1/schedules",
		`{"bot_id":"a","cron":"0 * * * *"}`, "t1", ""))
	var created cloudsched.ScheduledBot
	json.Unmarshal(w.Body.Bytes(), &created)

	// Cross-team delete → 404.
	w = httptest.NewRecorder()
	s.handleDeleteSchedule(w, scheduleReq(ctx, "DELETE",
		"/api/teams/t2/schedules/"+created.ID, "", "t2", created.ID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-team delete: code=%d", w.Code)
	}

	// Legit delete → 204.
	w = httptest.NewRecorder()
	s.handleDeleteSchedule(w, scheduleReq(ctx, "DELETE",
		"/api/teams/t1/schedules/"+created.ID, "", "t1", created.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s", w.Code, w.Body.String())
	}
	// Gone.
	w = httptest.NewRecorder()
	s.handleListSchedules(w, scheduleReq(ctx, "GET", "/api/teams/t1/schedules", "", "t1", ""))
	var lr struct {
		Schedules []cloudsched.ScheduledBot `json:"schedules"`
	}
	json.Unmarshal(w.Body.Bytes(), &lr)
	if len(lr.Schedules) != 0 {
		t.Fatalf("after delete: %d", len(lr.Schedules))
	}
}

// TestBuildScheduledLaunchSpec verifies the pure-data half of
// launchScheduledBot threads repo binding + vars + bot id onto the LaunchSpec
// so the cloud runner clones the pinned repo (mandatory for a stateful bot
// that persists state to git). The full launchScheduledBot path needs a live
// runs service; this focused unit protects the shape without that plumbing.
func TestBuildScheduledLaunchSpec(t *testing.T) {
	sb := cloudsched.ScheduledBot{
		ID: "sb-1", TenantID: "team-A", BotID: "feed-watch",
		Vars:    map[string]string{"mode": "digest"},
		RepoURL: "https://example.com/repo.git",
		RepoRef: "feat/x",
	}
	route := &store.RunLLMRoutePolicy{RefusedPinnedKey: "forfait"}
	spec := buildScheduledLaunchSpec(sb, "/tmp/feed-watch/main.bot", "workflow: {}", nil, route)
	if spec.FilePath != "/tmp/feed-watch/main.bot" || spec.Source != "workflow: {}" {
		t.Errorf("source pass-through: %+v", spec)
	}
	if spec.BotID != "feed-watch" || spec.Vars["mode"] != "digest" {
		t.Errorf("identity/vars: %+v", spec)
	}
	if spec.RepoURL != "https://example.com/repo.git" || spec.RepoRef != "feat/x" {
		t.Errorf("repo fields not threaded: %+v", spec)
	}
	// The scheduled path snapshots the route policy like every other
	// launch surface — a cron bot's run doc must answer "why did this run
	// route this way" too.
	if spec.LLMRoutePolicy != route {
		t.Errorf("LLMRoutePolicy = %+v, want the caller's resolution threaded", spec.LLMRoutePolicy)
	}
}

func TestSchedule_CreateResolvesRepoIntegration(t *testing.T) {
	s := newScheduleTestServer(t)
	s.forgeConnections = connection.NewMemoryForgeStore()
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	ctx := context.Background()
	if err := s.forgeConnections.Create(ctx, forge.Connection{
		ID: "conn-1", TenantID: "t1", Provider: forge.ProviderGitHub,
		ForgeBaseURL: "https://github.com", Status: forge.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(ctx, forge.RepoIntegration{
		ID: "ri-1", TenantID: "t1", ConnectionID: "conn-1",
		Provider: forge.ProviderGitHub, RepoFullName: "Acme/widgets",
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(superAdminCtx(), "POST", "/api/teams/t1/schedules",
		`{"bot_id":"feed-watch","cron":"*/5 * * * *","repo_url":"https://github.com/acme/widgets"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created cloudsched.ScheduledBot
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.RepoIntegrationID != "ri-1" {
		t.Errorf("RepoIntegrationID = %q, want ri-1 (URL-matched, case/scheme/.git-insensitive)", created.RepoIntegrationID)
	}

	// Unknown repo URL stays URL-bound (no integration id) — not an error.
	w2 := httptest.NewRecorder()
	s.handleCreateSchedule(w2, scheduleReq(superAdminCtx(), "POST", "/api/teams/t1/schedules",
		`{"bot_id":"feed-watch","cron":"*/5 * * * *","repo_url":"https://github.com/other/repo"}`, "t1", ""))
	if w2.Code != http.StatusCreated {
		t.Fatalf("create 2: code=%d", w2.Code)
	}
	var created2 cloudsched.ScheduledBot
	_ = json.Unmarshal(w2.Body.Bytes(), &created2)
	if created2.RepoIntegrationID != "" {
		t.Errorf("unknown repo should not bind an integration, got %q", created2.RepoIntegrationID)
	}
}

// newSchedulePinRouteServer is newScheduleTestServer with an api key store
// holding the shapes a schedule pin is judged on: t1's org-scoped gateway
// and anthropic keys, alice's personal key of t1, t2's key, and a key scoped
// to the identity the feed-watch schedule launches under.
func newSchedulePinRouteServer(t *testing.T) *Server {
	t.Helper()
	s := newScheduleTestServer(t)
	keys := secrets.NewMemoryApiKeyStore()
	s.apiKeys = keys
	seedSweepKey(t, keys, "t1", "key-tech", secrets.ProviderOpenAICompatible)
	seedSweepKey(t, keys, "t1", "key-design", secrets.ProviderOpenAICompatible)
	seedSweepKey(t, keys, "t1", "key-anthropic", secrets.ProviderAnthropic)
	seedSweepKey(t, keys, "t2", "key-t2", secrets.ProviderOpenAICompatible)
	for _, k := range []secrets.ApiKey{
		{ID: "key-alice", ScopeTeamID: "t1", ScopeUserID: "alice", Provider: secrets.ProviderOpenAICompatible},
		{ID: "key-feed-watch-lane", ScopeTeamID: "t1", ScopeUserID: scheduledLaunchOwner("feed-watch"), Provider: secrets.ProviderOpenAICompatible},
	} {
		k.Name, k.Fingerprint, k.CreatedAt = k.ID, "fp-"+k.ID, time.Now().UTC()
		if err := keys.Create(store.WithTenant(t.Context(), "t1"), k); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func createScheduleWith(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(superAdminCtx(), "POST", "/api/teams/t1/schedules", body, "t1", ""))
	return w
}

func patchScheduleWith(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(superAdminCtx(), "PATCH", "/api/teams/t1/schedules/"+id, body, "t1", id))
	return w
}

func storedKeyPins(t *testing.T, s *Server, id string) map[string]string {
	t.Helper()
	sb, err := s.cfg.ScheduledBots.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sb.KeyOverrides
}

// TestSchedule_CreateKeyOverrides: the create route stores a pin it admits,
// returns it on the list, and refuses — storing nothing — every pin the
// schedule's tick could not spend.
func TestSchedule_CreateKeyOverrides(t *testing.T) {
	s := newSchedulePinRouteServer(t)
	w := createScheduleWith(t, s, `{"bot_id":"feed-watch","cron":"0 6 * * *","key_overrides":{"openai_compatible":"key-design"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created cloudsched.ScheduledBot
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"openai_compatible": "key-design"}
	if got := storedKeyPins(t, s, created.ID); !maps.Equal(got, want) {
		t.Fatalf("stored key_overrides = %v, want %v", got, want)
	}
	lw := httptest.NewRecorder()
	s.handleListSchedules(lw, scheduleReq(superAdminCtx(), "GET", "/api/teams/t1/schedules", "", "t1", ""))
	var lr struct {
		Schedules []cloudsched.ScheduledBot `json:"schedules"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &lr); err != nil {
		t.Fatal(err)
	}
	if len(lr.Schedules) != 1 || !maps.Equal(lr.Schedules[0].KeyOverrides, want) {
		t.Fatalf("listed schedules = %+v, want the one row carrying %v", lr.Schedules, want)
	}

	for _, tc := range []struct{ name, pins, says string }{
		{"a personal key, invisible to the schedule's lane", `{"openai_compatible":"key-alice"}`, "not spendable"},
		{"another team's key", `{"openai_compatible":"key-t2"}`, "key-t2"},
		{"a key of another provider", `{"anthropic":"key-tech"}`, "is for provider"},
		{"a key that does not exist", `{"openai_compatible":"key-gone"}`, "not found"},
		{"a name the pin sweep cannot address", `{"openai.compatible":"key-tech"}`, "must not hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSchedulePinRouteServer(t)
			w := createScheduleWith(t, s, `{"bot_id":"feed-watch","cron":"0 6 * * *","key_overrides":`+tc.pins+`}`)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "key_overrides") || !strings.Contains(w.Body.String(), tc.says) {
				t.Fatalf("create = %d %s, want a 400 naming key_overrides and %q", w.Code, w.Body.String(), tc.says)
			}
			if rows, _ := s.cfg.ScheduledBots.ListByTenant(context.Background(), "t1"); len(rows) != 0 {
				t.Fatalf("a refused create stored %d row(s), want none", len(rows))
			}
		})
	}
}

// TestSchedule_KeyPinsAreJudgedForTheTicksIdentity: a pin is judged against
// the identity the schedule's own tick resolves keys under —
// scheduledLaunchOwner of the row's bot — so the write admits exactly the
// key the tick can spend: a key scoped to the feed-watch lane is admitted on
// a feed-watch schedule and refused on another bot's.
func TestSchedule_KeyPinsAreJudgedForTheTicksIdentity(t *testing.T) {
	s := newSchedulePinRouteServer(t)
	pin := `"key_overrides":{"openai_compatible":"key-feed-watch-lane"}`
	if w := createScheduleWith(t, s, `{"bot_id":"feed-watch","cron":"0 6 * * *",`+pin+`}`); w.Code != http.StatusCreated {
		t.Fatalf("a key the feed-watch tick spends, pinned on a feed-watch schedule = %d %s, want 201", w.Code, w.Body.String())
	}
	w := createScheduleWith(t, s, `{"bot_id":"nightly-audit","cron":"0 6 * * *",`+pin+`}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), scheduledLaunchOwner("nightly-audit")) {
		t.Fatalf("the same key on a nightly-audit schedule = %d %s, want a 400 naming %q", w.Code, w.Body.String(), scheduledLaunchOwner("nightly-audit"))
	}
	// The PATCH judges against the stored row's bot, the one its tick fires.
	cw := createScheduleWith(t, s, `{"bot_id":"nightly-audit","cron":"0 6 * * *"}`)
	var created cloudsched.ScheduledBot
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if pw := patchScheduleWith(t, s, created.ID, `{`+pin+`}`); pw.Code != http.StatusBadRequest {
		t.Fatalf("PATCH pinning the feed-watch lane's key on a nightly-audit row = %d %s, want 400", pw.Code, pw.Body.String())
	}
}

// TestSchedule_PatchKeyOverrides: absent leaves the pins, an object replaces
// them (validated), null and {} clear them, and an edit echoing the stored
// pins back is no write at all — not judged, so a pin whose key was deleted
// since does not refuse a pause (the tick refuses it by name instead).
func TestSchedule_PatchKeyOverrides(t *testing.T) {
	s := newSchedulePinRouteServer(t)
	w := createScheduleWith(t, s, `{"bot_id":"feed-watch","cron":"0 6 * * *","key_overrides":{"openai_compatible":"key-tech"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created cloudsched.ScheduledBot
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.ID
	tech := map[string]string{"openai_compatible": "key-tech"}
	design := map[string]string{"openai_compatible": "key-design", "anthropic": "key-anthropic"}

	step := func(name, body string, wantCode int, want map[string]string) {
		t.Helper()
		pw := patchScheduleWith(t, s, id, body)
		if pw.Code != wantCode {
			t.Fatalf("%s: PATCH %s = %d %s, want %d", name, body, pw.Code, pw.Body.String(), wantCode)
		}
		if got := storedKeyPins(t, s, id); !maps.Equal(got, want) {
			t.Fatalf("%s: stored key_overrides = %v, want %v", name, got, want)
		}
	}
	step("absent", `{"disabled":true}`, http.StatusOK, tech)
	step("replace", `{"key_overrides":{"openai_compatible":"key-design","anthropic":"key-anthropic"}}`, http.StatusOK, design)
	step("a refused replace stores nothing", `{"key_overrides":{"openai_compatible":"key-alice"}}`, http.StatusBadRequest, design)
	step("not an object of strings", `{"key_overrides":{"openai_compatible":1}}`, http.StatusBadRequest, design)
	step("null clears", `{"key_overrides":null}`, http.StatusOK, nil)
	step("re-pin", `{"key_overrides":{"openai_compatible":"key-tech"}}`, http.StatusOK, tech)
	step("{} clears", `{"key_overrides":{}}`, http.StatusOK, nil)
	step("re-pin again", `{"key_overrides":{"openai_compatible":"key-tech"}}`, http.StatusOK, tech)

	if err := s.apiKeys.Delete(store.WithTenant(context.Background(), "t1"), "key-tech"); err != nil {
		t.Fatal(err)
	}
	step("an echo of a pin whose key is gone pauses", `{"disabled":true,"key_overrides":{"openai_compatible":"key-tech"}}`, http.StatusOK, tech)
	step("a new pin on a gone key is refused", `{"key_overrides":{"anthropic":"key-tech"}}`, http.StatusBadRequest, tech)
}
