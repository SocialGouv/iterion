package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/credusage"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
)

// withFloor installs a reservation policy on the test server, through the
// same resolver production uses.
func withFloor(t *testing.T, s *Server, p budgetfloor.Policy) {
	t.Helper()
	if err := p.Validate(); err != nil {
		t.Fatalf("policy: %v", err)
	}
	st := platformcfg.NewMemoryStore[budgetfloor.Policy]()
	if err := st.Put(context.Background(), p); err != nil {
		t.Fatalf("put policy: %v", err)
	}
	s.budgetFloor = platformcfg.NewResolver(st, nil)
}

func seedRepoUsage(t *testing.T, c credusage.Counter, repo string, costUSD float64, runs int) {
	t.Helper()
	for i := 0; i < runs; i++ {
		if err := c.AddSpend(context.Background(), time.Now().UTC(), credusage.Spend{
			Key: credusage.Key{
				Fingerprint: "fp", Provider: "anthropic",
				Tier: credusage.TierTeam, TenantID: "t1", RepoID: repo,
			},
			Nature: credusage.NatureMetered, Backend: "claw",
			CostUSD: costUSD / float64(runs), InputTokens: 10,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// The second half of the ask: "a quota per repo within that global budget".
// It is a CEILING — the reservations answer what is held for a workload, this
// answers how far one repository may go — and it is read off the very meter
// the runs write, so the number `usage --by-credential --repo X` shows is the
// number this refuses on.
func TestGateLaunch_RepoQuota(t *testing.T) {
	newServer := func(t *testing.T, quota budgetfloor.RepoQuota) (*Server, context.Context) {
		t.Helper()
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		s.credUsage = credusage.NewMemoryCounter()
		withFloor(t, s, budgetfloor.Policy{RepoQuotas: []budgetfloor.RepoQuota{quota}})
		return s, seedGate(t, s, gateSpec{id: "t1"})
	}

	t.Run("a repository over its monthly amount is refused", func(t *testing.T) {
		s, ctx := newServer(t, budgetfloor.RepoQuota{Repo: "o/hungry", MonthlyUSD: 10})
		seedRepoUsage(t, s.credUsage, "o/hungry", 12.0, 3)

		_, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr", Repo: "o/hungry"})
		if d == nil || d.reason != denyRepoQuota {
			t.Fatalf("denial = %+v, want %s", d, denyRepoQuota)
		}
		if d.resetAt.IsZero() || !d.resetAt.After(time.Now()) {
			t.Fatalf("resetAt = %v, want the next month boundary", d.resetAt)
		}
	})

	t.Run("another repository is untouched by it", func(t *testing.T) {
		// The quota bounds ONE repository. A shared budget in which one repo's
		// overrun stopped every other repo would be a tenant cap wearing a
		// repository's name.
		s, ctx := newServer(t, budgetfloor.RepoQuota{Repo: "o/hungry", MonthlyUSD: 10})
		seedRepoUsage(t, s.credUsage, "o/hungry", 12.0, 3)

		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr", Repo: "o/quiet"}); d != nil {
			t.Fatalf("an unrelated repository was refused: %+v", d)
		}
		// And a run that names NO repository is not "every repository".
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr"}); d != nil {
			t.Fatalf("a run with no repository was refused by a repo quota: %+v", d)
		}
	})

	t.Run("under the quota it launches", func(t *testing.T) {
		s, ctx := newServer(t, budgetfloor.RepoQuota{Repo: "o/hungry", MonthlyUSD: 100})
		seedRepoUsage(t, s.credUsage, "o/hungry", 12.0, 3)
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr", Repo: "o/hungry"}); d != nil {
			t.Fatalf("refused at $12 of a $100 quota: %+v", d)
		}
	})

	t.Run("the activity axis refuses on metered route-spends, not amount", func(t *testing.T) {
		s, ctx := newServer(t, budgetfloor.RepoQuota{Repo: "o/busy", RouteSpendsPerMonth: 3})
		seedRepoUsage(t, s.credUsage, "o/busy", 0.03, 3) // cheap, but three charges
		_, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr", Repo: "o/busy"})
		if d == nil || d.reason != denyRepoQuota {
			t.Fatalf("denial = %+v, want %s on the activity count", d, denyRepoQuota)
		}
		// The number in the message is what the ledger counts — one unit per
		// (credential, backend, model) route an attempt charged, NOT a run.
		// The axis was called `runs_per_month` and its denial said "monthly
		// runs", so a repository configured for 100 was refused after ~30.
		if strings.Contains(d.detail, "monthly runs") || !strings.Contains(d.detail, "route-spends") {
			t.Errorf("detail = %q, want it to name metered route-spends rather than runs", d.detail)
		}
	})
}

// A reservation holds concurrency slots back from every OTHER workload, and
// none from its holder.
func TestGateLaunch_ConcurrencyReserve(t *testing.T) {
	// 3 slots, 2 held for the reviewer: ordinary work may take 1.
	policy := budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
		{BotID: "review-pr", Reserve: budgetfloor.Reserve{ConcurrentRuns: 2}},
	}}
	newServer := func(t *testing.T, active int) (*Server, context.Context) {
		t.Helper()
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		s.cfg.Store = fakeActiveStore{active: active}
		withFloor(t, s, policy)
		return s, seedGate(t, s, gateSpec{id: "t1", maxConcurrentRuns: 3})
	}

	t.Run("an ordinary bot stops at the unreserved slots", func(t *testing.T) {
		s, ctx := newServer(t, 1) // 1 active, ordinary ceiling is 3-2 = 1
		_, d := s.gateLaunch(ctx, launchSubject{BotID: "feature-dev"})
		if d == nil || d.reason != denyConcurrencyCap {
			t.Fatalf("denial = %+v, want %s — the reserved slots are not free", d, denyConcurrencyCap)
		}
	})

	t.Run("the reserved bot uses the whole team cap", func(t *testing.T) {
		s, ctx := newServer(t, 1)
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr"}); d != nil {
			t.Fatalf("the reserved bot was refused a slot it holds: %+v", d)
		}
	})

	t.Run("a reservation cannot create a concurrency cap", func(t *testing.T) {
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		s.cfg.Store = fakeActiveStore{active: 99}
		withFloor(t, s, policy)
		ctx := seedGate(t, s, gateSpec{id: "t1"}) // no maxConcurrentRuns
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "feature-dev"}); d != nil {
			t.Fatalf("refused on a team with NO concurrency cap: %+v — the reservation invented one", d)
		}
	})

	t.Run("the guarantee it makes, and the case it over-refuses", func(t *testing.T) {
		// The count is per TENANT, not per bot, so the rule enforced is
		// "unreserved work may not push the TOTAL past cap - reserved". What
		// it guarantees: the holder always finds its slots. What it costs:
		// while the holder spends its own reserve, an unreserved launch is
		// refused although the fleet is under its cap. Pinned deliberately —
		// the alternative (approximating the holder's usage) overcommits the
		// operator's cap, and the exact rule needs a per-bot active count the
		// store does not expose. If that count ever lands, this is the test
		// that should change.
		s, ctx := newServer(t, 2) // both active runs are the reviewer's
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr"}); d != nil {
			t.Fatalf("the holder was refused with a free slot: %+v", d)
		}
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "feature-dev"}); d == nil {
			t.Error("an unreserved bot was admitted past the conservative ceiling — the reserved slots would then not be guaranteed free")
		}
	})

	t.Run("reserves that take every slot refuse instead of waiting on a finish", func(t *testing.T) {
		// 2 slots, both held for the reviewer, and NOTHING running: the
		// ordinary bot must be refused, and told what to change.
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		s.cfg.Store = fakeActiveStore{active: 0}
		withFloor(t, s, policy)
		ctx := seedGate(t, s, gateSpec{id: "t1", maxConcurrentRuns: 2})
		_, d := s.gateLaunch(ctx, launchSubject{BotID: "feature-dev"})
		if d == nil || d.reason != denyConcurrencyCap {
			t.Fatalf("denial = %+v, want %s with every slot reserved", d, denyConcurrencyCap)
		}
		if !strings.Contains(d.detail, "reserved for other workloads") {
			t.Errorf("detail = %q, want it to name the reservation — no finishing run ever frees a reserved slot", d.detail)
		}
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr"}); d != nil {
			t.Fatalf("the holder was refused its own slots: %+v", d)
		}
	})
}

// The monthly-dollar reserve: real money on a metered key, and the same
// no-invented-ceiling guard as the other two axes.
func TestGateLaunch_MonthlyUSDReserve(t *testing.T) {
	policy := budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
		{BotID: "review-pr", Reserve: budgetfloor.Reserve{MonthlyUSD: 30}},
	}}

	t.Run("an ordinary bot stops at the unreserved dollars", func(t *testing.T) {
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		withFloor(t, s, policy)
		ctx := seedGate(t, s, gateSpec{id: "t1", orgCostCapUSD: 50})
		// $25 already spent: under the $50 cap, over the $20 an ordinary bot
		// may reach once $30 is held for the reviewer.
		if err := s.orgUsage.AddSpend(context.Background(), "t1", time.Now().UTC(), 25, 10, 10, 0); err != nil {
			t.Fatalf("seed spend: %v", err)
		}
		_, d := s.gateLaunch(ctx, launchSubject{BotID: "feature-dev"})
		if d == nil || d.reason != denyMonthlyCostCap {
			t.Fatalf("denial = %+v, want %s — $30 of the $50 is held for the reviewer", d, denyMonthlyCostCap)
		}
	})

	t.Run("the reserved bot reaches the whole cap", func(t *testing.T) {
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		withFloor(t, s, policy)
		ctx := seedGate(t, s, gateSpec{id: "t1", orgCostCapUSD: 50})
		if err := s.orgUsage.AddSpend(context.Background(), "t1", time.Now().UTC(), 25, 10, 10, 0); err != nil {
			t.Fatalf("seed spend: %v", err)
		}
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr"}); d != nil {
			t.Fatalf("the reserved bot was refused inside its own band: %+v", d)
		}
	})

	t.Run("a reservation cannot create a cost cap", func(t *testing.T) {
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		withFloor(t, s, policy)
		ctx := seedGate(t, s, gateSpec{id: "t1"}) // no cost cap at all
		if err := s.orgUsage.AddSpend(context.Background(), "t1", time.Now().UTC(), 999, 10, 10, 0); err != nil {
			t.Fatalf("seed spend: %v", err)
		}
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "feature-dev"}); d != nil {
			t.Fatalf("refused on an org with NO cost cap: %+v — the reservation invented one", d)
		}
	})

	// The inversion this axis is one keystroke from: orgusage gates on
	// `maxCostMillis > 0` in BOTH twins, so subtracting a reserve down to zero
	// does not hold unreserved work off — it turns the cost cap OFF for the
	// rest of the month, for exactly the workloads the reserve exists to hold
	// back. Spending NOTHING yet is what makes the test sharp: only a real
	// denial can refuse here.
	t.Run("a reserve that swallows the cost cap denies instead of unlimiting", func(t *testing.T) {
		s := newOrgTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		withFloor(t, s, policy) // $30 held for review-pr
		ctx := seedGate(t, s, gateSpec{id: "t1", orgCostCapUSD: 30})
		_, d := s.gateLaunch(ctx, launchSubject{BotID: "feature-dev"})
		if d == nil || d.reason != denyMonthlyCostCap {
			t.Fatalf("denial = %+v, want %s — the whole $30 cap is reserved for the reviewer", d, denyMonthlyCostCap)
		}
		if d.resetAt.IsZero() {
			t.Error("no reset instant on a monthly denial")
		}
		// The refusal happens BEFORE AllowRun, so it consumes no run slot.
		u, err := s.orgUsage.Usage(context.Background(), "t1", time.Now().UTC())
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		if u.Runs != 0 {
			t.Errorf("the denied launch metered %d run(s) — a run that never started consumes no monthly slot", u.Runs)
		}
		// And the holder still reaches its own band.
		if _, d := s.gateLaunch(ctx, launchSubject{BotID: "review-pr"}); d != nil {
			t.Fatalf("the reserved bot was refused inside its own reservation: %+v", d)
		}
	})
}

// The REST launch is the surface an operator uses by hand, and the subject it
// passes decides whether a reservation protects that operator's bot or is
// turned against it: judged as ordinary work, a RESERVED bot faces the ceiling
// its own reservation lowered, so the studio's Launch button is refused on the
// very band held for it. The body names the bot, so the gate is told.
func TestHandleLaunchRun_CarriesTheRequestsBotToTheGate(t *testing.T) {
	newSrv := func(t *testing.T, reserved string) (*Server, context.Context) {
		t.Helper()
		pub := &countingPublisher{}
		s, rs := newGatedBoardServer(t, gateSpec{id: "t1", maxConcurrentRuns: 2}, pub)
		s.cfg.Store = fakeActiveStore{RunStore: rs, active: 1}
		s.orgUsage = orgusage.NewMemoryCounter()
		pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
		withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
			{BotID: reserved, Reserve: budgetfloor.Reserve{ConcurrentRuns: 1}},
		}})
		// newGatedBoardServer already seeded org+team "t1"; the handler needs
		// the identity a signed-in operator would carry.
		return s, auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1", OrgID: "t1"})
	}
	launch := func(t *testing.T, s *Server, ctx context.Context, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleLaunchRun(rec, req)
		return rec
	}

	t.Run("the reserved bot is admitted on its own slot", func(t *testing.T) {
		s, ctx := newSrv(t, "probe")
		if rec := launch(t, s, ctx, `{"bot_id":"probe"}`); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("the reserved bot was refused on its own reservation: %s", rec.Body.String())
		}
	})

	t.Run("an unreserved bot still stops at the unreserved slots", func(t *testing.T) {
		s, ctx := newSrv(t, "review-pr")
		rec := launch(t, s, ctx, `{"bot_id":"probe"}`)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 — the free slot is held for review-pr: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a request too malformed to name a bot consumes no run slot", func(t *testing.T) {
		s, ctx := newSrv(t, "probe")
		if rec := launch(t, s, ctx, `{"nonsense":1}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		u, err := s.orgUsage.Usage(context.Background(), "t1", time.Now().UTC())
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		if u.Runs != 0 {
			t.Errorf("monthly runs = %d after a rejected body, want 0 — nothing was launched", u.Runs)
		}
	})
}
