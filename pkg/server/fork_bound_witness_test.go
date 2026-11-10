package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// ADR-162 decision 5's fail-closed arms and the bound's ladder, each driven
// on the production path and each isolated: one guard per scenario, so a
// mutant of the guard reddens its own row while every other arm still
// stands. The rows the delivery-row table names live in
// webhooks_fork_gesture_test.go; these are the arms behind the gate.

// forkGateSubject is the subject a fork delivery hands the gate: untrusted,
// keyed on the author's numeric forge id.
func forkGateSubject() launchSubject {
	return launchSubject{
		BotID: "review-pr", Repo: "acme/widgets",
		Trust:      store.RunTrustFork,
		ForkAuthor: &webhooks.ForkAuthorRef{Provider: "github", AuthorID: "4242"},
	}
}

// patchOrgForkBound sets the org's per-contributor bound through the REAL
// super-admin route (applyForkAuthorBound) and returns what it persisted.
func patchOrgForkBound(t *testing.T, s *Server, body string) (runs int, usd float64) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleAdminUpdateOrg(w, orgReq(superAdminCtx(), http.MethodPatch, "/api/admin/orgs/t1", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH %s: code=%d body=%s — the bound's ladder is set through this route", body, w.Code, w.Body.String())
	}
	o, err := s.authStore().GetOrg(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	return o.ForkAuthorRunQuota, o.ForkAuthorCostCapUSD
}

// TestGateLaunch_TheOrgLadderOfTheForkAuthorBound: the org's own bound
// (ADR-162 Q4), set through PATCH /api/admin/orgs/{id} and read by the gate
// (forkAuthorCaps): a POSITIVE value is the cap over the platform default,
// -1 is the explicit `unbounded` hatch (any negative normalises to it), and 0
// inherits the default. One row per rung, per axis.
//
// Mutations, each reddening its own row: forkAuthorCaps ignores a positive
// org run cap (the "cap" row admits the second run); treats a negative run
// field as inherit (the "hatch" row denies the second run); the same two on
// the dollar axis; applyForkAuthorBound refuses a negative like
// applyNonNegative (both hatch rows answer 400 at the PATCH).
func TestGateLaunch_TheOrgLadderOfTheForkAuthorBound(t *testing.T) {
	author := orgusage.ForkAuthorSubject("t1", "github", "4242")
	newGate := func(t *testing.T, defaults OrgLimitDefaults) (*Server, *orgusage.MemoryCounter, context.Context) {
		t.Helper()
		s := newOrgTestServer(t)
		c := orgusage.NewMemoryCounter()
		s.orgUsage = c
		s.orgDefaults = defaults
		return s, c, seedGate(t, s, gateSpec{id: "t1"})
	}
	admit := func(t *testing.T, s *Server, ctx context.Context) *launchDenial {
		t.Helper()
		_, d := s.gateLaunch(ctx, forkGateSubject())
		return d
	}

	t.Run("a positive org run cap overrides the platform default", func(t *testing.T) {
		s, _, ctx := newGate(t, OrgLimitDefaults{ForkAuthorRunQuota: 20, ForkAuthorCostCapUSD: 20})
		if runs, _ := patchOrgForkBound(t, s, `{"fork_author_run_quota":1}`); runs != 1 {
			t.Fatalf("persisted fork_author_run_quota = %d, want 1", runs)
		}
		if d := admit(t, s, ctx); d != nil {
			t.Fatalf("first fork run denied: %+v", d)
		}
		d := admit(t, s, ctx)
		if d == nil || d.reason != denyForkAuthorRuns {
			t.Fatalf("second fork run: denial = %+v, want %q — the org's own cap (1) binds, not the platform's 20", d, denyForkAuthorRuns)
		}
	})

	t.Run("a negative org run field is the unbounded hatch", func(t *testing.T) {
		s, _, ctx := newGate(t, OrgLimitDefaults{ForkAuthorRunQuota: 1, ForkAuthorCostCapUSD: 20})
		if runs, _ := patchOrgForkBound(t, s, `{"fork_author_run_quota":-5}`); runs != -1 {
			t.Fatalf("persisted fork_author_run_quota = %d, want -1 (the hatch, normalised)", runs)
		}
		for i := 0; i < 3; i++ {
			if d := admit(t, s, ctx); d != nil {
				t.Fatalf("fork run #%d denied (%+v) — the org opened the hatch, the platform's cap of 1 no longer binds", i+1, d)
			}
		}
	})

	t.Run("a positive org dollar cap overrides the platform default", func(t *testing.T) {
		s, c, ctx := newGate(t, OrgLimitDefaults{ForkAuthorRunQuota: 20, ForkAuthorCostCapUSD: 20})
		if _, usd := patchOrgForkBound(t, s, `{"fork_author_cost_cap_usd":1}`); usd != 1 {
			t.Fatalf("persisted fork_author_cost_cap_usd = %v, want 1", usd)
		}
		if err := c.AddSpend(context.Background(), author, time.Now().UTC(), 1.5, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		d := admit(t, s, ctx)
		if d == nil || d.reason != denyForkAuthorCost {
			t.Fatalf("denial = %+v, want %q — $1.50 spent is over the org's own $1, not the platform's $20", d, denyForkAuthorCost)
		}
	})

	t.Run("a negative org dollar field is the unbounded hatch", func(t *testing.T) {
		s, c, ctx := newGate(t, OrgLimitDefaults{ForkAuthorRunQuota: 20, ForkAuthorCostCapUSD: 1})
		if _, usd := patchOrgForkBound(t, s, `{"fork_author_cost_cap_usd":-1}`); usd != -1 {
			t.Fatalf("persisted fork_author_cost_cap_usd = %v, want -1 (the hatch)", usd)
		}
		if err := c.AddSpend(context.Background(), author, time.Now().UTC(), 5, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		if d := admit(t, s, ctx); d != nil {
			t.Fatalf("denied (%+v) — the org opened the dollar hatch, the platform's $1 no longer binds", d)
		}
	})

	// A positive dollar cap CostToMillis cannot carry reaches AllowRun as "no
	// cap": refused at the PATCH, nothing written, so the default still binds.
	// Mutation: drop applyForkAuthorCostCap's CostCapMillis refusal → the
	// PATCH persists the value and the gate admits $100 of spend.
	for _, body := range []string{`{"fork_author_cost_cap_usd":0.0004}`, `{"fork_author_cost_cap_usd":1e300}`} {
		t.Run("a dollar cap the counter cannot enforce is refused: "+body, func(t *testing.T) {
			s, c, ctx := newGate(t, OrgLimitDefaults{ForkAuthorRunQuota: 20, ForkAuthorCostCapUSD: 20})
			w := httptest.NewRecorder()
			s.handleAdminUpdateOrg(w, orgReq(superAdminCtx(), http.MethodPatch, "/api/admin/orgs/t1", body, "t1"))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s: code=%d body=%s, want 400 — the value is an implicit unbounded", body, w.Code, w.Body.String())
			}
			o, err := s.authStore().GetOrg(context.Background(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			if o.ForkAuthorCostCapUSD != 0 {
				t.Fatalf("persisted fork_author_cost_cap_usd = %v after a refused PATCH, want 0", o.ForkAuthorCostCapUSD)
			}
			if err := c.AddSpend(context.Background(), author, time.Now().UTC(), 100, 0, 0, 0); err != nil {
				t.Fatal(err)
			}
			if d := admit(t, s, ctx); d == nil || d.reason != denyForkAuthorCost {
				t.Fatalf("denial = %+v, want %q — the platform's $20 still binds", d, denyForkAuthorCost)
			}
		})
	}

	t.Run("0 inherits the platform default", func(t *testing.T) {
		s, _, ctx := newGate(t, OrgLimitDefaults{ForkAuthorRunQuota: 1, ForkAuthorCostCapUSD: 20})
		if runs, _ := patchOrgForkBound(t, s, `{"fork_author_run_quota":0}`); runs != 0 {
			t.Fatalf("persisted fork_author_run_quota = %d, want 0", runs)
		}
		if d := admit(t, s, ctx); d != nil {
			t.Fatalf("first fork run denied: %+v", d)
		}
		if d := admit(t, s, ctx); d == nil || d.reason != denyForkAuthorRuns {
			t.Fatalf("second fork run: denial = %+v, want %q — 0 inherits the platform's 1", d, denyForkAuthorRuns)
		}
	})
}

// authorOnlyErringCounter meters the org normally and fails ONLY on a fork
// author key — the input that reaches the author-side metering-error arm.
// (A counter failing every call stops at the ORG's arm first, so the author's
// arm would be covered by the org's and witnessed by neither.)
type authorOnlyErringCounter struct {
	*orgusage.MemoryCounter
	orgCalls atomic.Int32
}

func (c *authorOnlyErringCounter) AllowRun(ctx context.Context, subject orgusage.Subject, when time.Time, maxRuns int, maxCostMillis int64) (orgusage.DenyReason, error) {
	if strings.HasPrefix(string(subject), "forkauthor|") {
		return orgusage.DenyNone, errors.New("author counter is down")
	}
	c.orgCalls.Add(1)
	return c.MemoryCounter.AllowRun(ctx, subject, when, maxRuns, maxCostMillis)
}

// TestForkGesture_AnAuthorMeteringErrorRefusesAfterTheOrgCharged: decision
// 5's author-side metering-error arm, through a real delivery. The org's
// AllowRun SUCCEEDS (its unit is reserved), the author's fails: the launch is
// refused fail-closed — never admitted with the author's increment missing —
// and the org's reserved unit is handed back.
//
// Mutation: `ferr != nil && false` in gateMonthlyCaps → the delivery launches
// with no author increment (red on its own assertion).
func TestForkGesture_AnAuthorMeteringErrorRefusesAfterTheOrgCharged(t *testing.T) {
	fx := newForkGestureFixture(t)
	c := &authorOnlyErringCounter{MemoryCounter: fx.counter}
	fx.s.orgUsage = c
	w := fx.replay(t, 4101, "/revi")
	if c.orgCalls.Load() == 0 {
		t.Fatal("the org's metering was never asked — this row would be the org arm's, not the author's")
	}
	if fx.launchedCount() != 0 {
		t.Fatalf("a delivery whose author could not be metered launched %d run(s) — an untrusted launch is refused, not admitted with the author unmetered", fx.launchedCount())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body=%s: %v", w.Body.String(), err)
	}
	if resp["error_code"] != denyForkMeteringError {
		t.Fatalf("error_code = %q, want %q (body %s)", resp["error_code"], denyForkMeteringError, w.Body.String())
	}
	orgU, err := fx.counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if orgU.Runs != 0 {
		t.Fatalf("org runs = %d after the author's metering failed, want 0 — the org's reserved unit is handed back", orgU.Runs)
	}
}

// TestForkGesture_AnAbandonedAdmissionHandsTheAuthorUnitBack: the admission
// handle owns BOTH reservations (decision 5), so a launch abandoned after the
// gate admitted it — here a launch proven not to have started — releases the
// author's unit with the org's. An outside contributor's bound left holding a
// run that never was is a denial of service against the person it protects.
//
// Mutation: `a.forkSubject != "" && false` in launchAdmission.rollback → the
// author's unit stays charged (red on its own assertion).
func TestForkGesture_AnAbandonedAdmissionHandsTheAuthorUnitBack(t *testing.T) {
	fx := newForkGestureFixture(t)
	fx.launchErr = errors.New("compile refused: nothing durable exists")
	fx.replay(t, 4201, "/revi")
	if fx.launchedCount() != 1 {
		t.Fatalf("the launcher was asked %d time(s), want 1 — the row needs an admission that is then abandoned", fx.launchedCount())
	}
	now := time.Now().UTC()
	authorU, err := fx.counter.Usage(context.Background(), orgusage.ForkAuthorSubject("t1", "github", "4242"), now)
	if err != nil {
		t.Fatal(err)
	}
	if authorU.Runs != 0 {
		t.Fatalf("author runs = %d after an abandoned admission, want 0 — the rollback hands the author's unit back with the org's", authorU.Runs)
	}
	orgU, err := fx.counter.Usage(context.Background(), orgusage.OrgSubject("t1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if orgU.Runs != 0 {
		t.Fatalf("org runs = %d after an abandoned admission, want 0", orgU.Runs)
	}
}

// TestHandleResumeRun_AForkRunNamingNoAuthorIsRefused: the gate's
// nil-ForkAuthor arm. A fork run whose document names no author subject (a
// write that lost it, a document older than the field) has nothing to meter
// the per-contributor bound on: its resume is refused fail-closed —
// fork_metering_unavailable, nothing metered, nothing published — never
// admitted on the org's budget alone.
//
// Mutation: turn the nil-ForkAuthor arm of gateMonthlyCaps into a skip → the
// resume is admitted (202) on the org's unit only (red on its own assertion).
func TestHandleResumeRun_AForkRunNamingNoAuthorIsRefused(t *testing.T) {
	fx := newForkResumeFixture(t, store.RunStatusPausedOperator)
	seedForkResumeRunAs(t, fx.s, fx.rs, "run-fork-unkeyed", store.RunStatusPausedOperator, "")
	rec := httptest.NewRecorder()
	fx.s.handleResumeRun(rec, orgReq(fx.ctx, http.MethodPost, "/api/runs/run-fork-unkeyed/resume", `{}`, "run-fork-unkeyed"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s), want 503 — a fork run with no author to meter is not resumed on the org's budget alone", rec.Code, rec.Body.String())
	}
	var body struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body.ErrorCode != denyForkMeteringUnavailable {
		t.Fatalf("error_code = %q, want %q", body.ErrorCode, denyForkMeteringUnavailable)
	}
	if n := fx.pub.resumed(); n != 0 {
		t.Fatalf("%d resume(s) published past the refusal", n)
	}
	orgU, err := fx.counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if orgU.Runs != 0 {
		t.Fatalf("org runs = %d after the refusal, want 0", orgU.Runs)
	}
}

// TestRunFork_AForkedForkChildResumesMeteredOnTheSameAuthor: POST
// /api/runs/{id}/fork is the operator's recovery path for a failed run, and
// the child of a FORK run is that author's spend. runview's Fork is the second
// constructor of a run from a parent (CreateChildRun is the first): it carries
// the trust AND the author subject through store.InheritTrustAndAuthor, so the
// child's gated resume re-enters the SAME per-contributor bound — the author
// metered beside the org — instead of being refused forever as an untrusted
// run with no author.
//
// Mutation: restore the trust-only carry in runview.Service.Fork
// (`child.Trust = parent.Trust`) → the child's resume is refused 503
// fork_metering_unavailable (red on its own assertion).
func TestRunFork_AForkedForkChildResumesMeteredOnTheSameAuthor(t *testing.T) {
	fx := newForkResumeFixture(t, store.RunStatusFailedResumable)
	ctx := context.Background()
	if err := fx.rs.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: "run-fork", NodeID: "done", LoopIter: 0, TurnIndex: 0,
		Backend: "claw", FinishReason: "end_turn", WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write turn: %v", err)
	}
	res, err := fx.s.runs.Fork(ctx, runview.ForkSpec{RunID: "run-fork", NodeID: "done", TurnIndex: -1})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := fx.rs.LoadRun(ctx, res.NewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Trust != store.RunTrustFork {
		t.Fatalf("child trust = %q — the carry under test did not run, so this row proves nothing", child.Trust)
	}
	rec := httptest.NewRecorder()
	fx.s.handleResumeRun(rec, orgReq(fx.ctx, http.MethodPost, "/api/runs/"+res.NewRunID+"/resume", `{}`, res.NewRunID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("child resume: status = %d (%s), want 202 — the child of a fork run resumes on its author's bound", rec.Code, rec.Body.String())
	}
	assertForkTwinMetered(t, fx.counter)
}

// TestForkGesture_TheRealGitHubClientsUnidentifiedAuthorIsRefused: the "no
// numeric author id" row, with the REAL GitHub client decoding the forge's
// answer behind the gesture — a stub returning "" is gentler than the producer
// it stands in for. A pull request whose user carries no id (or is null) is
// refused on the gesture's own row, and nothing is metered on a key shared by
// every unidentified author.
//
// Mutation: githubUserID renders an absent id as "0" (the int64 decode) →
// the gesture launches on the shared subject forkauthor|t1|github|0 (red on
// its own assertion).
func TestForkGesture_TheRealGitHubClientsUnidentifiedAuthorIsRefused(t *testing.T) {
	for _, row := range []struct {
		name string
		user any
	}{
		{name: "a user without an id", user: map[string]any{"login": "alice"}},
		{name: "a null user", user: nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 7, "title": "Add X", "state": "open",
					"html_url": "https://github.com/acme/widgets/pull/7",
					"head": map[string]any{
						"ref": "feat/x", "sha": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
						"repo": map[string]any{"full_name": "alice/widgets-fork", "clone_url": "https://github.com/alice/widgets-fork.git"},
					},
					"base": map[string]any{"ref": "main", "repo": map[string]any{
						"clone_url": "https://github.com/acme/widgets.git", "private": false, "visibility": "public",
					}},
					"user": row.user,
				})
			}))
			defer api.Close()
			fx := newForkGestureFixture(t)
			client := &forgegithub.AdminClient{HTTP: api.Client(), APIBase: api.URL, Token: "t"}
			fx.forge.getPR = func(ctx context.Context, repo string, number int) (forge.PullRef, error) {
				return client.GetPullRequest(ctx, repo, number)
			}
			w := fx.replay(t, 4301, "/revi")
			if fx.launchedCount() != 0 {
				t.Fatalf("a pull request the forge names no author id for launched %d run(s)", fx.launchedCount())
			}
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s — a refusal is a 200/filtered verdict", w.Code, w.Body.String())
			}
			rows, err := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("delivery rows=%+v err=%v, want the refusal recorded", rows, err)
			}
			if !strings.Contains(rows[0].Error, "no numeric author id") {
				t.Fatalf("delivery reason = %q, want the gesture's own no-author row", rows[0].Error)
			}
			shared, err := fx.counter.Usage(context.Background(), orgusage.ForkAuthorSubject("t1", "github", "0"), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if shared.Runs != 0 {
				t.Fatalf("the shared id-0 subject metered %d run(s)", shared.Runs)
			}
		})
	}
}
