package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Platform runs view tests (#2148): the cross-org listing is
// super-admin only, names each row's team and org, marks sovereign
// pools, paginates server-side across orgs, and audits every
// super-admin cross-team read through ONE named predicate.

// newAdminRunsServer boots the production New()/routes()/middleware
// stack with orgs, teams and runs seeded across two orgs, and returns
// a super-admin plus a plain team-admin bearer. Runs are seeded through
// a second store handle on the SAME directory the server's runview
// opened — the filesystem store reads per call, so files landed after
// boot are visible to the routes.
func newAdminRunsServer(t *testing.T) (audit.Store, *Server, *httptest.Server, string, string) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	ident := identity.NewMemoryStore()
	svc, err := auth.NewService(auth.Config{
		Store:      ident,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	auditStore := audit.NewMemoryStore()
	cfg := Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		Audit:                   auditStore,
	}
	s := New(cfg, iterlog.New(iterlog.LevelError, nil))

	seedPlatformAdmin(t, s.authStore(), "root")
	for _, o := range []identity.Org{
		{ID: "org-alpha", Name: "Alpha Org", Slug: "alpha"},
		{ID: "org-beta", Name: "Beta Org", Slug: "beta"},
	} {
		if _, err := ident.CreateOrg(context.Background(), o); err != nil {
			t.Fatalf("seed org %s: %v", o.ID, err)
		}
	}
	for _, tm := range []identity.Team{
		{ID: "team-a1", OrgID: "org-alpha", Name: "Team A1", Slug: "a1"},
		{ID: "team-a2", OrgID: "org-alpha", Name: "Team A2", Slug: "a2"},
		{ID: "team-b1", OrgID: "org-beta", Name: "Team B1", Slug: "b1"},
	} {
		if _, err := ident.CreateTeam(context.Background(), tm); err != nil {
			t.Fatalf("seed team %s: %v", tm.ID, err)
		}
	}

	st, err := store.New(store.ResolveStoreDir(cfg.WorkDir, ""))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for _, seed := range []struct {
		id, team, pool, status string
		at                     time.Time
	}{
		{"run-a1x", "team-a1", "", "finished", base},
		{"run-a1f", "team-a1", "", "failed", base.Add(time.Second)},
		{"run-a2p", "team-a2", "pool-souverain", "failed", base.Add(2 * time.Second)},
		{"run-b1x", "team-b1", "", "finished", base.Add(3 * time.Second)},
	} {
		if err := st.SaveRun(context.Background(), &store.Run{
			ID:           seed.id,
			Name:         "run " + seed.id,
			WorkflowName: "wf-" + seed.id,
			Status:       store.RunStatus(seed.status),
			TenantID:     seed.team,
			RunnerPool:   seed.pool,
			CreatedAt:    seed.at,
			UpdatedAt:    seed.at,
		}); err != nil {
			t.Fatalf("seed run %s: %v", seed.id, err)
		}
	}

	adminTok, _, err := signer.IssueAccess(auth.Identity{UserID: "root", IsSuperAdmin: true, TeamID: "team-root"})
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	userTok, _, err := signer.IssueAccess(auth.Identity{UserID: "u1", TeamID: "team-a1", Role: identity.RoleAdmin})
	if err != nil {
		t.Fatalf("issue user token: %v", err)
	}
	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return auditStore, s, hs, adminTok, userTok
}

// adminRunsGet requests the cross-org list and decodes the page.
func adminRunsGet(t *testing.T, hs *httptest.Server, token, query string) (int, adminRunsResponse) {
	t.Helper()
	code, body := llmDo(t, hs, "GET", "/api/admin/runs"+query, token, "")
	var out adminRunsResponse
	if code == 200 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode admin runs response: %v (body %s)", err, body)
		}
	}
	return code, out
}

// countPlatformEvents counts the audit rows of one action — the read
// the waiters below poll.
func countPlatformEvents(t *testing.T, st audit.Store, action string) int {
	t.Helper()
	events, err := st.ListPlatform(context.Background(), audit.Page{})
	if err != nil {
		t.Fatalf("list platform audit: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// waitForAuditEvent polls the platform audit until one event matches —
// the insert is best-effort and detached, so the assertion waits for
// the effect instead of racing it.
func waitForAuditEvent(t *testing.T, st audit.Store, match func(audit.Event) bool) audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := st.ListPlatform(context.Background(), audit.Page{})
		if err != nil {
			t.Fatalf("list platform audit: %v", err)
		}
		for _, e := range events {
			if match(e) {
				return e
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the audited event never landed in the platform audit")
	return audit.Event{}
}

// TestAdminRuns_SuperAdminOnly: a plain team admin — owner of a team,
// but not an instance super-admin — gets 403 and no rows through the
// production stack; the super-admin gets the view. The route carries
// TWO belts (requireSuperAdmin, then the predicate) — this witness
// proves the BEHAVIOUR; TestAdminRuns_PredicateRefusesNonSuperAdmin
// isolates the second belt at the only altitude it is observable.
func TestAdminRuns_SuperAdminOnly(t *testing.T) {
	_, _, hs, adminTok, userTok := newAdminRunsServer(t)
	code, _ := adminRunsGet(t, hs, userTok, "")
	if code != 403 {
		t.Fatalf("team admin got %d on the cross-org runs list, want 403 super-admin only", code)
	}
	code, _ = adminRunsGet(t, hs, adminTok, "")
	if code != 200 {
		t.Fatalf("super-admin got %d, want 200", code)
	}
}

// TestAdminRuns_PredicateRefusesNonSuperAdmin isolates the ONE named
// predicate's own refusal: the request enters at HANDLER altitude (no
// middleware in front of it), so the predicate is the only guard
// standing — the only altitude at which that belt is observable at
// all. A 200 here would mean the predicate is decoration.
func TestAdminRuns_PredicateRefusesNonSuperAdmin(t *testing.T) {
	_, s, _, _, _ := newAdminRunsServer(t)
	req := httptest.NewRequest("GET", "/api/admin/runs", nil).WithContext(auth.WithIdentity(context.Background(), auth.Identity{
		UserID: "u1", TeamID: "team-a1", Role: identity.RoleAdmin,
	}))
	w := httptest.NewRecorder()
	s.handleAdminListRuns(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("the predicate answered %d to a non-super-admin at handler altitude, want 403", w.Code)
	}
}

// TestAdminRuns_RowsNameTheirOrgAndMarkSovereignPools: each row names
// the team and org it belongs to — resolved server-side, no active-org
// dependency — and a run stamped to a sovereign runner pool carries
// the stamp the console marks visually (A19).
func TestAdminRuns_RowsNameTheirOrgAndMarkSovereignPools(t *testing.T) {
	_, _, hs, adminTok, _ := newAdminRunsServer(t)
	code, out := adminRunsGet(t, hs, adminTok, "")
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	if len(out.Runs) != 4 {
		t.Fatalf("got %d rows, want the 4 seeded runs across both orgs", len(out.Runs))
	}
	byID := map[string]adminRunRow{}
	for _, row := range out.Runs {
		byID[row.ID] = row
	}
	row, ok := byID["run-a2p"]
	if !ok {
		t.Fatal("the sovereign-pool run is missing from the view")
	}
	if row.TeamID != "team-a2" || row.TeamName != "Team A2" || row.OrgID != "org-alpha" || row.OrgName != "Alpha Org" {
		t.Fatalf("row identity incomplete: %+v", row)
	}
	if row.RunnerPool != "pool-souverain" {
		t.Fatalf("sovereign pool stamp lost: %+v", row)
	}
	if b := byID["run-b1x"]; b.TeamName != "Team B1" || b.OrgName != "Beta Org" {
		t.Fatalf("second org not named on its rows: %+v", b)
	}
}

// TestAdminRuns_FiltersCombine: org and team narrowing and the status
// filter combine (never accepted-and-dropped, #1419); a team outside
// the org answers an EMPTY page — the combination honoured, not the
// org alone.
func TestAdminRuns_FiltersCombine(t *testing.T) {
	_, _, hs, adminTok, _ := newAdminRunsServer(t)

	code, out := adminRunsGet(t, hs, adminTok, "?org_id=org-alpha")
	if code != 200 || len(out.Runs) != 3 {
		t.Fatalf("org filter: code %d rows %d, want 200 and the org's 3 runs", code, len(out.Runs))
	}
	for _, row := range out.Runs {
		if row.TeamID != "team-a1" && row.TeamID != "team-a2" {
			t.Fatalf("org filter leaked team %s", row.TeamID)
		}
	}

	code, out = adminRunsGet(t, hs, adminTok, "?org_id=org-alpha&status=failed")
	if code != 200 || len(out.Runs) != 2 {
		t.Fatalf("org+status combined: code %d rows %d, want the org's 2 failed runs", code, len(out.Runs))
	}
	failedSeen := map[string]bool{}
	for _, row := range out.Runs {
		if row.TeamID != "team-a1" && row.TeamID != "team-a2" {
			t.Fatalf("org+status filter leaked team %s", row.TeamID)
		}
		failedSeen[row.ID] = true
	}
	if !failedSeen["run-a2p"] || !failedSeen["run-a1f"] {
		t.Fatalf("org+status combined missed a failed run: %v", failedSeen)
	}

	code, out = adminRunsGet(t, hs, adminTok, "?org_id=org-alpha&team_id=team-b1")
	if code != 200 || len(out.Runs) != 0 {
		t.Fatalf("org+foreign team: code %d rows %d, want 200 and an empty page", code, len(out.Runs))
	}

	code, out = adminRunsGet(t, hs, adminTok, "?team_id=team-b1")
	if code != 200 || len(out.Runs) != 1 || out.Runs[0].ID != "run-b1x" {
		t.Fatalf("team filter alone: code %d rows %+v, want exactly run-b1x", code, out.Runs)
	}

	code, _ = adminRunsGet(t, hs, adminTok, "?org_id=org-nope")
	if code != 404 {
		t.Fatalf("unknown org: code %d, want 404", code)
	}

	code, _ = adminRunsGet(t, hs, adminTok, "?cursor=not-a-cursor")
	if code != 400 {
		t.Fatalf("malformed cursor: code %d, want 400", code)
	}
}

// TestAdminRuns_KeysetPaginationWalksEveryRunExactlyOnce: walking the
// cursor two rows at a time sees every seeded run exactly once and
// terminates — the page walk is correct across orgs, not merely
// non-empty.
func TestAdminRuns_KeysetPaginationWalksEveryRunExactlyOnce(t *testing.T) {
	_, _, hs, adminTok, _ := newAdminRunsServer(t)
	seen := map[string]bool{}
	query := "?limit=2"
	for pages := 0; ; pages++ {
		code, out := adminRunsGet(t, hs, adminTok, query)
		if code != 200 {
			t.Fatalf("page %d (%s): %d", pages, query, code)
		}
		for _, row := range out.Runs {
			if seen[row.ID] {
				t.Fatalf("run %s seen twice across pages", row.ID)
			}
			seen[row.ID] = true
		}
		if out.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatalf("pagination never terminates after %d pages", pages)
		}
		query = "?limit=2&cursor=" + out.NextCursor
	}
	if len(seen) != 4 {
		t.Fatalf("walked %d unique runs, want all 4 seeded (seen: %v)", len(seen), seen)
	}
}

// TestAdminRuns_CrossOrgReadsAreAudited: the ordinary per-team paths
// leave no cross-org audit line; the super-admin list crosses the
// named predicate once per request; and OPENING a foreign-org run by
// id — the "no active-org error" path — serves the super-admin through
// the ADR-103 choke point and is audited there too.
func TestAdminRuns_CrossOrgReadsAreAudited(t *testing.T) {
	auditStore, _, hs, adminTok, userTok := newAdminRunsServer(t)

	before := countPlatformEvents(t, auditStore, "run.cross_org_list")
	code, _ := adminRunsGet(t, hs, userTok, "")
	if code != 403 {
		t.Fatalf("member on the cross-org list: %d, want 403", code)
	}
	// A member on the ORDINARY route (its own active team) is served —
	// and unaudited: the flag that reaches everywhere is the one that
	// must leave a trace, not the membership everyone has.
	code, body := llmDo(t, hs, "GET", "/api/runs", userTok, "")
	if code != 200 {
		t.Fatalf("member's own-team list: %d (body %s), want 200", code, body)
	}
	if got := countPlatformEvents(t, auditStore, "run.cross_org_list"); got != before {
		t.Fatalf("ordinary per-team reads audited %d cross-org lines, want %d", got, before)
	}

	// The super-admin list crosses the predicate once per request.
	if code, _ := adminRunsGet(t, hs, adminTok, ""); code != 200 {
		t.Fatalf("super-admin list: %d, want 200", code)
	}
	ev := waitForAuditEvent(t, auditStore, func(e audit.Event) bool {
		return e.Action == "run.cross_org_list" && e.ActorKind == "super_admin"
	})
	if ev.ActorID != "root" {
		t.Fatalf("cross-org list audit line names %q, want the acting super-admin", ev.ActorID)
	}

	// Opening a foreign-org run by id: served without an active-org
	// error (the choke point re-anchors the super-admin on the run's
	// team), and the grant is audited with the run it opened.
	code, body = llmDo(t, hs, "GET", "/api/runs/run-b1x", adminTok, "")
	if code != 200 {
		t.Fatalf("opening a foreign-org run answered %d (%s), want 200 without an active-org error", code, body)
	}
	waitForAuditEvent(t, auditStore, func(e audit.Event) bool {
		return e.Action == "run.cross_team_read" && e.TargetID == "run-b1x"
	})
}
