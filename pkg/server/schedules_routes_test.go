package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
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
	spec := buildScheduledLaunchSpec(sb, "/tmp/feed-watch/main.bot", "workflow: {}", nil)
	if spec.FilePath != "/tmp/feed-watch/main.bot" || spec.Source != "workflow: {}" {
		t.Errorf("source pass-through: %+v", spec)
	}
	if spec.BotID != "feed-watch" || spec.Vars["mode"] != "digest" {
		t.Errorf("identity/vars: %+v", spec)
	}
	if spec.RepoURL != "https://example.com/repo.git" || spec.RepoRef != "feat/x" {
		t.Errorf("repo fields not threaded: %+v", spec)
	}
}

func TestSchedule_CreateResolvesRepoIntegration(t *testing.T) {
	s := newScheduleTestServer(t)
	s.forgeConnections = forge.NewMemoryConnectionStore()
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
