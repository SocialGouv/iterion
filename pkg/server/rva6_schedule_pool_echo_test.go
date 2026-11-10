package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// r6ScheduleWithProjectedBlock seeds an approval-scoped server with an org
// pool and a schedule row carrying the integration's PROJECTED block.
func r6ScheduleWithProjectedBlock(t *testing.T, block string) (*Server, *credpool.MemoryPoolStore, func()) {
	t.Helper()
	ctx := context.Background()
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	seedTeamKey(t, s, "t1")
	s.cfg.ScheduledBots = cloudsched.NewMemoryStore()
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	s.scheduleClock = func() time.Time { return now }
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-o1", OrgID: "o1", Enabled: true,
		OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
		t.Fatal(err)
	}
	stored, _, err := s.decodeAndValidatePoolBlock(ctx, "t1", json.RawMessage(block))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.ScheduledBots.Create(ctx, cloudsched.ScheduledBot{ID: "sched-1", TenantID: "t1",
		RepoIntegrationID: "ri-1", BotID: "review-pr", Cron: "*/5 * * * *", NextFireAt: now.Add(5 * time.Minute),
		Pool: stored, PoolFromIntegration: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return s, pools, done
}

// E1 — the echo compares triggers as a set: a client that echoes the
// stored block with its triggers in another order says the same thing — no
// pool write, the pause lands for a team admin (no review) and an org
// admin's echo keeps the projected provenance.
func TestPoolReview6_E1_aReorderedTriggerEchoIsNoChange(t *testing.T) {
	ctx := context.Background()
	stored := `{"pool_id":"pool-o1","enabled":true,"strategy":{"triggers":["unavailable","usage_window"]}}`
	echo := `{"pool_id":"pool-o1","enabled":true,"strategy":{"triggers":["usage_window","unavailable"]}}`
	for _, who := range []string{"team-admin", "org-admin"} {
		t.Run(who, func(t *testing.T) {
			s, _, done := r6ScheduleWithProjectedBlock(t, stored)
			defer done()
			cx := teamAdminCtx()
			if who == "org-admin" {
				cx = orgAdminCtx()
			}
			w := httptest.NewRecorder()
			s.handleUpdateSchedule(w, scheduleReq(cx, "PATCH", "/api/teams/t1/schedules/sched-1",
				`{"disabled":true,"pool":`+echo+`}`, "t1", "sched-1"))
			got, _ := s.cfg.ScheduledBots.Get(ctx, "sched-1")
			t.Logf("%s PATCH {disabled, pool: reordered echo} -> %d %s; row disabled=%v pool_from_integration=%v",
				who, w.Code, w.Body.String(), got.Disabled, got.PoolFromIntegration)
			if w.Code != 200 || !got.Disabled || !got.PoolFromIntegration {
				t.Errorf("a block naming the same triggers in another order was judged a change")
			}
		})
	}
}

// E1b — a default WRITTEN is a change: the stored block leaves the mode
// absent (it follows the default), the PATCH pins "priority" — a team
// admin's write of an enabled block reaches the review gate (409) and the
// row keeps its absent mode.
func TestPoolReview6_E1b_aWrittenDefaultIsAChange(t *testing.T) {
	ctx := context.Background()
	stored := `{"pool_id":"pool-o1","enabled":true}`
	s, _, done := r6ScheduleWithProjectedBlock(t, stored)
	defer done()
	w := httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(teamAdminCtx(), "PATCH", "/api/teams/t1/schedules/sched-1",
		`{"pool":{"pool_id":"pool-o1","enabled":true,"strategy":{"mode":"priority"}}}`, "t1", "sched-1"))
	got, _ := s.cfg.ScheduledBots.Get(ctx, "sched-1")
	if w.Code != 409 || got.Pool == nil || got.Pool.Strategy.Mode != "" {
		t.Errorf("pinning the default mode passed as an echo: code=%d stored mode=%q", w.Code, got.Pool.Strategy.Mode)
	}
}

// E2 — the echo is recognised BEFORE it is validated as a write: once the
// stored block's pool is disabled (or its consent withdrawn), a client that
// sends the whole record back still pauses the schedule — there is no pool
// write to refuse.
func TestPoolReview6_E2_anEchoOfAStaleBlockRefusesThePause(t *testing.T) {
	ctx := context.Background()
	block := `{"pool_id":"pool-o1","enabled":true}`
	s, pools, done := r6ScheduleWithProjectedBlock(t, block)
	defer done()
	p, _ := pools.Get(ctx, "pool-o1")
	p.Enabled = false
	if err := pools.Upsert(ctx, p); err != nil {
		t.Fatal(err)
	}
	w1 := httptest.NewRecorder()
	s.handleUpdateSchedule(w1, scheduleReq(orgAdminCtx(), "PATCH", "/api/teams/t1/schedules/sched-1",
		`{"disabled":true,"pool":`+block+`}`, "t1", "sched-1"))
	got, _ := s.cfg.ScheduledBots.Get(ctx, "sched-1")
	t.Logf("pool disabled; PATCH {disabled, pool: exact stored block} -> %d %s; row disabled=%v", w1.Code, w1.Body.String(), got.Disabled)
	w2 := httptest.NewRecorder()
	s.handleUpdateSchedule(w2, scheduleReq(orgAdminCtx(), "PATCH", "/api/teams/t1/schedules/sched-1",
		`{"disabled":true}`, "t1", "sched-1"))
	t.Logf("same PATCH without the block -> %d", w2.Code)
	if w1.Code != 200 || !got.Disabled {
		t.Errorf("an unchanged echo (no pool write) refused the whole edit: %d", w1.Code)
	}
}

// E3 — control: the exact echo plus a cron change by a team admin passes,
// the cron lands, the block and its provenance stay.
func TestPoolReview6_E3_controlExactEchoWithACronChange(t *testing.T) {
	ctx := context.Background()
	block := `{"pool_id":"pool-o1","enabled":true,"strategy":{"margin_pct":85,"triggers":["usage_window"]}}`
	s, _, done := r6ScheduleWithProjectedBlock(t, block)
	defer done()
	w := httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(teamAdminCtx(), "PATCH", "/api/teams/t1/schedules/sched-1",
		`{"cron":"0 * * * *","vars":{"k":"v"},"pool":{"pool_id":"pool-o1","enabled":true,"strategy":{"margin_pct":85.0,"triggers":["usage_window"]}}}`, "t1", "sched-1"))
	got, _ := s.cfg.ScheduledBots.Get(ctx, "sched-1")
	t.Logf("team admin PATCH {cron, vars, exact echo} -> %d; cron=%q vars=%v pool=%+v from_integration=%v", w.Code, got.Cron, got.Vars, got.Pool, got.PoolFromIntegration)
	if w.Code != 200 || got.Cron != "0 * * * *" || got.Vars["k"] != "v" || !got.PoolFromIntegration || got.Pool == nil {
		t.Errorf("exact echo + cron change: code=%d cron=%q provenance=%v", w.Code, got.Cron, got.PoolFromIntegration)
	}
}

// E4 — no false echo: a block differing from the stored one in a single
// strategy dial (reset_hold, margin 85 vs absent) is a change — the team
// admin's PATCH reaches the review gate (409) and nothing is written.
func TestPoolReview6_E4_aOneDialChangeIsNoEcho(t *testing.T) {
	ctx := context.Background()
	stored := `{"pool_id":"pool-o1","enabled":true,"strategy":{"triggers":["usage_window"]}}`
	for name, patch := range map[string]string{
		"reset_hold": `{"pool_id":"pool-o1","enabled":true,"strategy":{"triggers":["usage_window"],"reset_hold":true}}`,
		"margin":     `{"pool_id":"pool-o1","enabled":true,"strategy":{"triggers":["usage_window"],"margin_pct":85}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, done := r6ScheduleWithProjectedBlock(t, stored)
			defer done()
			w := httptest.NewRecorder()
			s.handleUpdateSchedule(w, scheduleReq(teamAdminCtx(), "PATCH", "/api/teams/t1/schedules/sched-1",
				`{"pool":`+patch+`}`, "t1", "sched-1"))
			got, _ := s.cfg.ScheduledBots.Get(ctx, "sched-1")
			t.Logf("team admin PATCH changing %s -> %d; row strategy=%+v from_integration=%v", name, w.Code, got.Pool.Strategy, got.PoolFromIntegration)
			if w.Code != 409 || got.Pool.Strategy.ResetHold || got.Pool.Strategy.MarginPct != nil {
				t.Errorf("a one-dial change passed as an echo: %d", w.Code)
			}
		})
	}
}

// E5 — a CHANGED block is still validated as a write: a PATCH naming a pool
// that does not exist, or one disabled since, is refused 400 and the row
// keeps its stored block.
func TestPoolReview6_E5_aChangedBlockIsValidated(t *testing.T) {
	ctx := context.Background()
	stored := `{"pool_id":"pool-o1","enabled":true}`
	for name, patch := range map[string]string{
		"unknown pool":  `{"pool_id":"pool-nope","enabled":true}`,
		"disabled pool": `{"pool_id":"pool-off","enabled":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, pools, done := r6ScheduleWithProjectedBlock(t, stored)
			defer done()
			if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-off", OrgID: "o1", Enabled: false,
				OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.handleUpdateSchedule(w, scheduleReq(orgAdminCtx(), "PATCH", "/api/teams/t1/schedules/sched-1",
				`{"pool":`+patch+`}`, "t1", "sched-1"))
			got, _ := s.cfg.ScheduledBots.Get(ctx, "sched-1")
			if w.Code != 400 || got.Pool == nil || got.Pool.PoolID != "pool-o1" {
				t.Errorf("PATCH binding %s: code=%d stored pool=%+v, want 400 and the stored block kept", name, w.Code, got.Pool)
			}
		})
	}
}
