package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The gate refuses on PRESENCE: a team admin's PATCH mirroring the stored
// (org-admin-bound) pool block back unchanged is refused 409, where the
// same PATCH without the block passes — unlike the guard above it, which
// refuses only a CHANGE because "every edit dialog sends the whole policy back".
func TestPoolGuarantee_S1_unchangedPoolBlockOnAPatchIsRefused(t *testing.T) {
	ctx := context.Background()
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
	seedTeamKey(t, s, "t1")
	s.cfg.ScheduledBots = cloudsched.NewMemoryStore()
	s.scheduleClock = func() time.Time { return time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC) }
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-o1", OrgID: "o1", Enabled: true,
		OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleCreateSchedule(w, scheduleReq(orgAdminCtx(), "POST", "/api/teams/t1/schedules",
		`{"bot_id":"review-pr","cron":"*/5 * * * *","pool":{"pool_id":"pool-o1","enabled":true}}`, "t1", ""))
	rows, _ := s.cfg.ScheduledBots.ListByTenant(ctx, "t1")
	if w.Code != 201 || len(rows) != 1 {
		t.Fatalf("org admin create: %d %s", w.Code, w.Body.String())
	}
	sid := rows[0].ID
	w1 := httptest.NewRecorder()
	s.handleUpdateSchedule(w1, scheduleReq(teamAdminCtx(), "PATCH", "/api/teams/t1/schedules/"+sid,
		`{"disabled":true}`, "t1", sid))
	w2 := httptest.NewRecorder()
	s.handleUpdateSchedule(w2, scheduleReq(teamAdminCtx(), "PATCH", "/api/teams/t1/schedules/"+sid,
		`{"disabled":true,"pool":{"pool_id":"pool-o1","enabled":true}}`, "t1", sid))
	t.Logf("team admin pause without the block: %d; with the STORED block mirrored back unchanged: %d %s", w1.Code, w2.Code, w2.Body.String())
	if w1.Code == 200 && w2.Code == 409 {
		t.Errorf("an unchanged pool block on a PATCH is refused — the gate judges presence, not change")
	}
}
