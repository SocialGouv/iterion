package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Regressions for consent defects an adversarial review found. Both let a
// contributor's subscription be used in a way they had not agreed to.

// A parked credential must not return to service just because its donor
// touched their terms. Only a genuine reconnection clears it — otherwise
// other people's runs rediscover the same dead token, one at a time.
func TestReconnected(t *testing.T) {
	parked := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		health    credpool.Health
		connected time.Time // rec.CreatedAt — stamped only by a real connect
		refreshed time.Time // rec.UpdatedAt — bumped hourly by the worker
		want      bool
	}{
		{"a healthy pledge is always clear", credpool.HealthOK, parked.Add(-time.Hour), parked.Add(time.Hour), true},
		{"an unset health is treated as healthy", "", parked.Add(-time.Hour), parked.Add(time.Hour), true},
		{
			"parked, credential untouched — stays parked",
			credpool.HealthAuthFailed, parked.Add(-time.Hour), parked.Add(-time.Hour), false,
		},
		{
			// THE defect: a provider can keep rotating tokens for a
			// subscription it has revoked. Trusting the refresh timestamp
			// let a dead credential un-park itself every hour and burn
			// another round of borrowers' runs on the same 401.
			"parked, only the token refresh moved — stays parked",
			credpool.HealthAuthFailed, parked.Add(-time.Hour), parked.Add(6 * time.Hour), false,
		},
		{
			"parked, genuinely reconnected since — cleared",
			credpool.HealthAuthFailed, parked.Add(time.Minute), parked.Add(time.Minute), true,
		},
		{
			"an expired token that was reconnected — cleared",
			credpool.HealthTokenExpired, parked.Add(time.Minute), parked.Add(time.Minute), true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := credpool.Pledge{Health: tc.health, UpdatedAt: parked}
			rec := secrets.OAuthRecord{CreatedAt: tc.connected, UpdatedAt: tc.refreshed}
			if got := reconnected(p, rec); got != tc.want {
				t.Errorf("reconnected = %v, want %v", got, tc.want)
			}
		})
	}
}

// A contribution joins the pool of the donor's OWN org, never a stranger's.
// A "the instance has only one pool, use that" fallback would attach a
// personal subscription to an org the donor never chose.
func TestPoolIDForUser_neverAttachesToAnotherOrgsPool(t *testing.T) {
	ctx := context.Background()
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-other", OrgID: "org-other", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Server{credPoolPools: pools, logger: iterlog.New(iterlog.LevelError, nil)}

	// A user of an org with no pool of its own gets nothing, even though
	// exactly one pool exists on the instance.
	r := httptest.NewRequest("GET", "/api/me/pool", nil)
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "alice", OrgID: "org-mine"}))
	if got := s.poolIDForUser(r); got != "" {
		t.Errorf("pool = %q, want none — alice was enrolled into another org's pool", got)
	}

	// Their own org's pool resolves normally.
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-mine", OrgID: "org-mine", Enabled: true}); err != nil {
		t.Fatalf("seed own: %v", err)
	}
	if got := s.poolIDForUser(r); got != "pool-mine" {
		t.Errorf("pool = %q, want pool-mine", got)
	}

	// No active org at all → nothing.
	r2 := httptest.NewRequest("GET", "/api/me/pool", nil)
	r2 = r2.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "bob"}))
	if got := s.poolIDForUser(r2); got != "" {
		t.Errorf("pool = %q, want none for a user with no active org", got)
	}
}

// The donor roster is the contributors' business, not every org member's.
func TestToPoolView_hidesTheRosterFromNonManagers(t *testing.T) {
	ctx := context.Background()
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("alice", "claude_code"), PoolID: "pool-1",
		UserID: "alice", Kind: "claude_code", Enabled: true, Health: credpool.HealthOK,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Server{
		credPoolPledges: pledges,
		credPoolLedger:  credpool.NewMemoryLedger(),
		logger:          iterlog.New(iterlog.LevelError, nil),
	}
	r := httptest.NewRequest("GET", "/api/teams/team-1/pool", nil)
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "carol"}))
	pool := credpool.Pool{ID: "pool-1", OrgID: "org-1", Enabled: true}

	if v := s.toPoolView(r, pool, false); len(v.Donors) != 0 {
		t.Errorf("a non-manager saw %d donor(s) — who lends is not org-wide public information", len(v.Donors))
	}
	if v := s.toPoolView(r, pool, true); len(v.Donors) != 1 {
		t.Errorf("a manager saw %d donor(s), want 1", len(v.Donors))
	}
}
