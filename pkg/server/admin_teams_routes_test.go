package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

// The F2 core at the route layer: the delegated caps route is a PATCH —
// mapping the team to a sovereign pool and then PATCHing its caps must
// leave the mapping standing. On the MEMORY twin this row is behavioural
// only (value aliasing cannot express a stale-read erase — rva F5); the
// bite lives in the mongo leg of the identity patch conformance and in
// TestUpdateTeamHasNoProductionCallers, which makes the whole-doc revert
// uncommittable.
func TestOrgTeamCaps_PatchPreservesTheRunnerPool(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()

	pool := "honorabilite"
	if _, err := s.authStore().PatchTeam(context.Background(), "t1", identity.TeamPatch{RunnerPool: &pool}); err != nil {
		t.Fatalf("map pool: %v", err)
	}

	req := orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/teams/t1/caps", `{"max_concurrent_runs":2}`, "o1")
	req.SetPathValue("team_id", "t1")
	w := httptest.NewRecorder()
	s.handleUpdateOrgTeamCaps(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("caps patch should pass: code=%d body=%s", w.Code, w.Body.String())
	}

	tm, err := s.authStore().GetTeam(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if tm.RunnerPool != "honorabilite" {
		t.Fatalf("the caps patch erased the pool mapping: %+v", tm)
	}
	if tm.MaxConcurrentRuns != 2 {
		t.Fatalf("the caps patch did not apply: %+v", tm)
	}
}

// The only writer of the mapping: the super-admin route. Set, clear, and
// grammar-refuse — audited, and refused for a team that does not exist.
func TestAdminSetTeamRunnerPool(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()

	call := func(body string) *httptest.ResponseRecorder {
		req := orgReq(superAdminCtx(), http.MethodPut, "/api/admin/teams/t1/runner-pool", body, "t1")
		w := httptest.NewRecorder()
		s.handleAdminSetTeamRunnerPool(w, req)
		return w
	}

	s.auditStore = audit.NewMemoryStore()
	if w := call(`{"runner_pool":"honorabilite"}`); w.Code != http.StatusOK {
		t.Fatalf("map: code=%d body=%s", w.Code, w.Body.String())
	}
	if tm, err := s.authStore().GetTeam(context.Background(), "t1"); err != nil || tm.RunnerPool != "honorabilite" {
		t.Fatalf("map did not stick: %+v %v", tm, err)
	}
	// The trail lands in BOTH scopes (rva-2 F3: both calls could be deleted
	// and the suite stayed green): the tenant row keyed by the team, the org
	// mirror keyed by the org — each carrying the previous value.
	// The insert rides goSafe (async) — poll for the rows instead of
	// asserting on the tick.
	var rows, orgRows []audit.Event
	for i := 0; i < 100; i++ {
		rows, _ = s.auditStore.ListByTenant(context.Background(), "t1", audit.Page{Action: "team.runner_pool_set"})
		orgRows, _ = s.auditStore.ListByTenant(context.Background(), "o1", audit.Page{Action: "team.runner_pool_set"})
		if len(rows) == 1 && len(orgRows) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("tenant trail: want 1 row, got %d", len(rows))
	}
	if len(orgRows) != 1 {
		t.Fatalf("org mirror: want 1 row, got %d", len(orgRows))
	}
	if got := orgRows[0].Meta["previous"]; got != "" {
		t.Fatalf("the trail must answer what the team WAS, got previous=%v", got)
	}

	if w := call(`{"runner_pool":""}`); w.Code != http.StatusOK {
		t.Fatalf("unmap: code=%d body=%s", w.Code, w.Body.String())
	}
	if tm, err := s.authStore().GetTeam(context.Background(), "t1"); err != nil || tm.RunnerPool != "" {
		t.Fatalf("unmap did not stick: %+v %v", tm, err)
	}

	if w := call(`{"runner_pool":"Bad_Name"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a malformed name must 422: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := call(`{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a missing runner_pool must 400: code=%d body=%s", w.Code, w.Body.String())
	}

	req := orgReq(superAdminCtx(), http.MethodPut, "/api/admin/teams/ghost/runner-pool", `{"runner_pool":"p"}`, "ghost")
	w := httptest.NewRecorder()
	s.handleAdminSetTeamRunnerPool(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown team must 404: code=%d body=%s", w.Code, w.Body.String())
	}
}

// The mapping route consults the REGISTRY: a pool it does not know is
// refused (422) even though the grammar is fine — mapping to a pool that
// does not exist would refuse every launch forever. Wiring a registry on
// the test server proves both arms. Red when the existence check is
// dropped.
func TestAdminSetTeamRunnerPool_RequiresARegisteredPool(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	s.runnerPoolsStore = platformcfg.NewMemoryStore[platformcfg.RunnerPools]()
	if err := s.runnerPoolsStore.Put(context.Background(), platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{
		{Name: "honorabilite", State: platformcfg.RunnerPoolProvisioning},
	}}); err != nil {
		t.Fatal(err)
	}

	call := func(pool string) *httptest.ResponseRecorder {
		req := orgReq(superAdminCtx(), http.MethodPut, "/api/admin/teams/t1/runner-pool", `{"runner_pool":"`+pool+`"}`, "t1")
		w := httptest.NewRecorder()
		s.handleAdminSetTeamRunnerPool(w, req)
		return w
	}
	if w := call("ghost-pool"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unregistered pool must 422: code=%d body=%s", w.Code, w.Body.String())
	}
	// A PROVISIONING pool maps fine — the launch refuses until active, the
	// mapping itself is the operator's intent and may precede the topology.
	if w := call("honorabilite"); w.Code != http.StatusOK {
		t.Fatalf("a registered pool must map: code=%d body=%s", w.Code, w.Body.String())
	}
}
