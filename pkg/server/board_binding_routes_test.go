package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// The team board-binding endpoints. The properties worth pinning are the ones
// a later refactor could break invisibly: the tenant boundary (a member of one
// team must not read or rewrite another's binding), and that a PUT actually
// RESOLVES the board rather than storing whatever the caller claimed.

func newBoardBindingTestServer(t *testing.T, bc forge.BoardClient) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	s.boardBindings = forge.NewMemoryBoardBindingStore()
	s.forgeConnections = forge.NewMemoryConnectionStore()
	// conn-1 belongs to t1; conn-other belongs to a DIFFERENT tenant. Both are
	// resolvable by id — the store's Get is global by design — which is exactly
	// what makes the ownership check the binding's own responsibility.
	seedConn(t, s, "conn-1", "t1")
	seedConn(t, s, "conn-other", "team-other")
	s.boardClientForBinding = func(context.Context, forge.BoardBinding) (forge.BoardClient, error) {
		return bc, nil
	}
	s.boardClientForConnection = func(context.Context, string) (forge.BoardClient, forge.Provider, error) {
		return bc, forge.ProviderGitHub, nil
	}
	return s
}

func seedConn(t *testing.T, s *Server, id, tenant string) {
	t.Helper()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: id, TenantID: tenant, Provider: forge.ProviderGitHub, Kind: forge.KindPAT,
	}); err != nil {
		t.Fatalf("seed connection %s: %v", id, err)
	}
}

// seedBoardAppConn seeds a GitHub-App connection whose installation reports the
// given grant set — the shape the bind-time permission probe reads.
func seedBoardAppConn(t *testing.T, s *Server, id, tenant string, granted map[string]string) {
	t.Helper()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: id, TenantID: tenant, Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		InstallationID: 42,
	}); err != nil {
		t.Fatalf("seed app connection %s: %v", id, err)
	}
	s.forgeInstallationGrants = func(context.Context, forge.Connection) (map[string]string, error) {
		return granted, nil
	}
}

func bindingReq(ctx context.Context, method, path, body, teamID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	r.SetPathValue("id", teamID)
	return r
}

func TestBoardBindingPutResolvesAndStores(t *testing.T) {
	bc := &bindRouteFake{project: routeBoardProject()}
	s := newBoardBindingTestServer(t, bc)
	ctx := superAdminCtx()

	body := `{"owner":"SocialGouv","number":203,"connection_id":"conn-1"}`
	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(ctx, "PUT", "/api/teams/t1/board-binding", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("put: code=%d body=%s", w.Code, w.Body.String())
	}
	var got forge.BoardBinding
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The ids must come from the BOARD, not from the request: a caller that
	// could name a project id could point a team at someone else's board.
	if got.ProjectID != "PVT_p" || got.StatusFieldID != "PVTSSF_status" {
		t.Fatalf("the PUT must resolve the board, got %+v", got)
	}
	if got.TenantID != "t1" {
		t.Errorf("TenantID = %q, want the path's team", got.TenantID)
	}
	if got.SyncEvery != forge.DefaultBoardSyncEvery {
		t.Errorf("SyncEvery = %v, want the default", got.SyncEvery)
	}

	// And it reads back.
	w = httptest.NewRecorder()
	s.handleGetBoardBinding(w, bindingReq(ctx, "GET", "/api/teams/t1/board-binding", "", "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("get: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBoardBindingGetIsNotFoundWhenUnbound(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})
	w := httptest.NewRecorder()
	s.handleGetBoardBinding(w, bindingReq(superAdminCtx(), "GET", "/api/teams/t1/board-binding", "", "t1"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 for a team with no board", w.Code)
	}
}

func TestBoardBindingPutAcceptsAStatusMapAndInterval(t *testing.T) {
	p := routeBoardProject()
	p.Fields[0].Options = []forge.ProjectFieldOption{
		{ID: "o_todo", Name: "Todo"}, {ID: "o_done", Name: "Done"},
	}
	s := newBoardBindingTestServer(t, &bindRouteFake{project: p})

	body := `{"owner":"SocialGouv","number":203,"connection_id":"conn-1",
	          "status_map":{"Todo":"ready","Done":"done"},"sync_every_seconds":300}`
	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(superAdminCtx(), "PUT", "/api/teams/t1/board-binding", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("put: code=%d body=%s", w.Code, w.Body.String())
	}
	var got forge.BoardBinding
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.StatusOptions["ready"] != "o_todo" {
		t.Errorf("the operator's map must win: %+v", got.StatusOptions)
	}
	if got.SyncEvery != 5*time.Minute {
		t.Errorf("SyncEvery = %v, want 5m", got.SyncEvery)
	}
}

func TestBoardBindingPutRefusesABadRequest(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})
	for _, tc := range []struct{ name, body string }{
		{"no owner", `{"number":203,"connection_id":"c"}`},
		{"no number", `{"owner":"SocialGouv","connection_id":"c"}`},
		{"no connection", `{"owner":"SocialGouv","number":203}`},
		{"non-injective map", `{"owner":"SocialGouv","number":203,"connection_id":"c","status_map":{"Planned":"ready","Inbox":"ready"}}`},
		{"interval under the floor", `{"owner":"SocialGouv","number":203,"connection_id":"c","sync_every_seconds":10}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handlePutBoardBinding(w, bindingReq(superAdminCtx(), "PUT", "/api/teams/t1/board-binding", tc.body, "t1"))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestBoardBindingDelete(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(ctx, "PUT", "/api/teams/t1/board-binding",
		`{"owner":"SocialGouv","number":203,"connection_id":"conn-1"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleDeleteBoardBinding(w, bindingReq(ctx, "DELETE", "/api/teams/t1/board-binding", "", "t1"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: code = %d, want 204", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleGetBoardBinding(w, bindingReq(ctx, "GET", "/api/teams/t1/board-binding", "", "t1"))
	if w.Code != http.StatusNotFound {
		t.Errorf("after delete: code = %d, want 404", w.Code)
	}
	// Deleting an absent binding is 404, not a silent success.
	w = httptest.NewRecorder()
	s.handleDeleteBoardBinding(w, bindingReq(ctx, "DELETE", "/api/teams/t1/board-binding", "", "t1"))
	if w.Code != http.StatusNotFound {
		t.Errorf("second delete: code = %d, want 404", w.Code)
	}
}

// TestBoardBindingIsTenantScoped is the boundary that matters: a caller with
// no rights on the team must neither read nor rewrite its board binding.
func TestBoardBindingIsTenantScoped(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})
	// Seed a binding for t1 as super-admin.
	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(superAdminCtx(), "PUT", "/api/teams/t1/board-binding",
		`{"owner":"SocialGouv","number":203,"connection_id":"conn-1"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}

	outsider := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u2", TeamID: "other-team"})
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"GET":    s.handleGetBoardBinding,
		"PUT":    s.handlePutBoardBinding,
		"DELETE": s.handleDeleteBoardBinding,
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			body := ""
			if name == "PUT" {
				body = `{"owner":"Evil","number":1,"connection_id":"c"}`
			}
			call(w, bindingReq(outsider, name, "/api/teams/t1/board-binding", body, "t1"))
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 for a non-member", w.Code)
			}
		})
	}
	// And t1's binding is untouched.
	b, err := s.boardBindings.GetByTenant(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if b.Owner != "SocialGouv" {
		t.Errorf("an outsider rewrote the binding: %+v", b)
	}
}

// TestBoardBindingRefusesAnotherTenantsConnection is the credential boundary.
// The team id comes from the PATH (and is authorized), but the connection id
// comes from the BODY — and forge.ConnectionStore.Get is keyed on the id
// alone, with no tenant filter, so an id from another team resolves fine.
//
// Without the ownership check, an admin of team A names team B's connection
// and gets a PERSISTED binding on B's credential: the sync worker then keeps
// reading B's org project and, worse, calls SetSingleSelect on it — writing
// Status fields on another organisation's board, indefinitely.
func TestBoardBindingRefusesAnotherTenantsConnection(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})

	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(superAdminCtx(), "PUT", "/api/teams/t1/board-binding",
		`{"owner":"SocialGouv","number":203,"connection_id":"conn-other"}`, "t1"))
	if w.Code == http.StatusOK {
		t.Fatalf("bound another tenant's connection: %s", w.Body.String())
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
	// Non-enumerating, like the peer routes: "not found", never "not yours".
	if body := w.Body.String(); !strings.Contains(body, "not found") {
		t.Errorf("the refusal must not confirm the connection exists, got %q", body)
	}
	// And nothing was persisted — a stored binding is what the worker would
	// keep using.
	if _, err := s.boardBindings.GetByTenant(context.Background(), "t1"); err == nil {
		t.Fatal("a refused bind must persist nothing")
	}
}

// TestBoardBindingRefusesAnUnknownConnection keeps the same shape for an id
// that exists nowhere, so the two cases are indistinguishable to a caller.
func TestBoardBindingRefusesAnUnknownConnection(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})
	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(superAdminCtx(), "PUT", "/api/teams/t1/board-binding",
		`{"owner":"SocialGouv","number":203,"connection_id":"conn-nope"}`, "t1"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "not found") {
		t.Errorf("body = %q", body)
	}
}

// TestBoardClientForBoundBindingRechecksOwnership covers what happens AFTER a
// legitimate bind: the connection is deleted, or re-created under another
// tenant. The stored binding still names it, and the sync worker writes
// through it — so ownership is re-asserted at use time, not only at write.
func TestBoardClientForBoundBindingRechecksOwnership(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})
	ctx := context.Background()

	// A binding the bind path would have accepted.
	ok := forge.BoardBinding{TenantID: "t1", ConnectionID: "conn-1"}
	if _, err := s.boardClientForBoundBinding(ctx, ok); err != nil {
		t.Fatalf("an owned connection must resolve: %v", err)
	}

	// The same binding after the connection moved to another tenant.
	foreign := forge.BoardBinding{TenantID: "t1", ConnectionID: "conn-other"}
	if _, err := s.boardClientForBoundBinding(ctx, foreign); err == nil {
		t.Fatal("the worker must refuse a credential the team does not own")
	}

	// And after it is deleted outright — a dangling credential is not a
	// licence to keep writing.
	if err := s.forgeConnections.Delete(ctx, "conn-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.boardClientForBoundBinding(ctx, ok); err == nil {
		t.Fatal("a deleted connection must refuse, not resolve")
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func routeBoardProject() forge.Project {
	return forge.Project{
		ID: "PVT_p", Number: 203, Title: "Iterion",
		Fields: []forge.ProjectField{
			{ID: "PVTSSF_status", Name: "Status", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{
				{ID: "o_inbox", Name: "Inbox"}, {ID: "o_planned", Name: "Planned"},
				{ID: "o_prog", Name: "In progress"}, {ID: "o_blocked", Name: "Blocked"},
				{ID: "o_done", Name: "Done"},
			}},
			{ID: "PVTSSF_area", Name: "Area", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{{ID: "a1", Name: "engine"}}},
		},
	}
}

type bindRouteFake struct {
	project forge.Project
	err     error
}

func (f *bindRouteFake) GetProject(context.Context, forge.ProjectRef) (forge.Project, error) {
	if f.err != nil {
		return forge.Project{}, f.err
	}
	return f.project, nil
}
func (f *bindRouteFake) ListProjectItems(context.Context, forge.ProjectRef, forge.ProjectItemListOptions) (forge.ProjectItemPage, error) {
	return forge.ProjectItemPage{}, nil
}
func (f *bindRouteFake) ItemForIssue(context.Context, forge.ProjectRef, string, int) (forge.ProjectItem, bool, error) {
	return forge.ProjectItem{}, false, nil
}
func (f *bindRouteFake) IssueContentID(context.Context, string, int) (string, error) { return "", nil }
func (f *bindRouteFake) AddItem(context.Context, string, string) (forge.ProjectItem, error) {
	return forge.ProjectItem{}, nil
}
func (f *bindRouteFake) SetSingleSelect(context.Context, string, string, string, string) error {
	return nil
}

// TestBoardBindingPutNamesAMissingProjectGrant pins the diagnostic on the
// feature's most likely first-run failure. GitHub answers a project the token
// cannot see with NOT_FOUND, so a credential missing the org-level
// organization_projects grant is indistinguishable — by its symptom — from a
// mistyped board number. Telling an operator to check their number when the
// real cause is a permission they have to have an org owner approve costs the
// whole afternoon the helper was written to save.
func TestBoardBindingPutNamesAMissingProjectGrant(t *testing.T) {
	// The board answers exactly as GitHub does for an invisible project.
	bc := &bindRouteFake{project: routeBoardProject(), err: forge.ErrProjectNotFound}
	s := newBoardBindingTestServer(t, bc)
	// conn-app is a GitHub App installation whose owner never approved the
	// org-level projects grant.
	seedBoardAppConn(t, s, "conn-app", "t1", map[string]string{"contents": "write", "metadata": "read"})

	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(superAdminCtx(), "PUT", "/api/teams/t1/board-binding",
		`{"owner":"SocialGouv","number":203,"connection_id":"conn-app"}`, "t1"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 — body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "organization_projects") {
		t.Errorf("400 body = %s\nwant the missing grant NAMED — %q on its own sends the operator to check their board number",
			body, "project not found")
	}
}

// TestBoardBindingPutAllowsAGrantedApp is the other half: the probe must not
// become a second way to refuse a working credential. An installation that
// HOLDS the grant binds exactly as a PAT does.
func TestBoardBindingPutAllowsAGrantedApp(t *testing.T) {
	s := newBoardBindingTestServer(t, &bindRouteFake{project: routeBoardProject()})
	seedBoardAppConn(t, s, "conn-app", "t1", map[string]string{"organization_projects": "write", "metadata": "read"})

	w := httptest.NewRecorder()
	s.handlePutBoardBinding(w, bindingReq(superAdminCtx(), "PUT", "/api/teams/t1/board-binding",
		`{"owner":"SocialGouv","number":203,"connection_id":"conn-app"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 — body=%s", w.Code, w.Body.String())
	}
}
