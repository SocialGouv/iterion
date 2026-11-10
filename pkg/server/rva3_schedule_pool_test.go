package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// Class check of the round-2 drawsShared doctrine: under shared_credentials,
// a self-funded team's admin binding the org's pool is PARKED on the forge
// enable — and the same binding on a schedule (the other record surface that
// writes a pool block) lands directly.
func TestPoolReview3_S1_scheduleBindsTheOrgPoolWithoutTheReview(t *testing.T) {
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
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":true}}`, "t1"))
	parked, _ := s.provisionApprovals.ListByOrg(ctx, "o1")
	t.Logf("forge enable binding pool-o1 (team admin, self-funded): code=%d parked=%d", w.Code, len(parked))

	w2 := httptest.NewRecorder()
	s.handleCreateSchedule(w2, scheduleReq(teamAdminCtx(), "POST", "/api/teams/t1/schedules",
		`{"bot_id":"review-pr","cron":"*/5 * * * *","pool":{"pool_id":"pool-o1","enabled":true}}`, "t1", ""))
	rows, _ := s.cfg.ScheduledBots.ListByTenant(ctx, "t1")
	parked2, _ := s.provisionApprovals.ListByOrg(ctx, "o1")
	for _, r := range rows {
		t.Logf("schedule create binding pool-o1 (same admin): code=%d schedule=%s pool=%+v parked=%d", w2.Code, r.ID, r.Pool, len(parked2))
	}
	if w.Code == http.StatusAccepted && w2.Code == http.StatusCreated {
		t.Errorf("the schedule surface binds the org's shared pool for unattended recurring runs with no review, where the forge enable parks the same binding")
	}
}
