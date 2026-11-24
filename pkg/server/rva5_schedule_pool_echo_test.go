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

// S1 (round 5) — the PATCH gate skips an UNCHANGED block, but the write
// still lands: applySchedulePatch drops PoolFromIntegration. A team admin
// (no org review power) echoing the block an approved integration
// PROJECTED onto its schedule row turns it into the row's OWN block, which
// survives the integration's later clear (orchestrator_pool_test: "operator
// block survives a clear") — the org admin's revocation at the integration
// level no longer reaches the schedule, and no review saw the adoption.
func TestPoolReview5_S1_anEchoedProjectedBlockIsAdoptedWithoutReview(t *testing.T) {
	ctx := context.Background()
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
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
	block := `{"pool_id":"pool-o1","enabled":true}`
	stored, _, err := s.decodeAndValidatePoolBlock(ctx, "t1", json.RawMessage(block))
	if err != nil {
		t.Fatal(err)
	}
	// The approved integration's block, projected onto its schedule row.
	if err := s.cfg.ScheduledBots.Create(ctx, cloudsched.ScheduledBot{ID: "sched-1", TenantID: "t1",
		RepoIntegrationID: "ri-1", BotID: "review-pr", Cron: "*/5 * * * *", NextFireAt: now.Add(5 * time.Minute),
		Pool: stored, PoolFromIntegration: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Control: the same team admin binding that pool on a schedule is refused.
	wc := httptest.NewRecorder()
	s.handleCreateSchedule(wc, scheduleReq(teamAdminCtx(), "POST", "/api/teams/t1/schedules",
		`{"bot_id":"review-pr","cron":"*/5 * * * *","pool":`+block+`}`, "t1", ""))

	w := httptest.NewRecorder()
	s.handleUpdateSchedule(w, scheduleReq(teamAdminCtx(), "PATCH", "/api/teams/t1/schedules/sched-1",
		`{"pool":`+block+`}`, "t1", "sched-1"))
	got, err := s.cfg.ScheduledBots.Get(ctx, "sched-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("team admin: create binding pool-o1 -> %d; PATCH echoing the projected block -> %d; row pool=%+v pool_from_integration=%v",
		wc.Code, w.Code, got.Pool, got.PoolFromIntegration)
	if w.Code == 200 && got.Pool != nil && !got.PoolFromIntegration {
		t.Errorf("the echo adopted the integration's reviewed binding as the row's own (provenance flag dropped) — the integration's clear no longer removes it")
	}
}
