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
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

func routingPolicyReq(ctx context.Context, method, path, body, id string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	if id != "" {
		r.SetPathValue("id", id)
	}
	return r
}

func asUser(userID string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: userID})
}

// putRoute dispatches the PUT to the org or team handler by URL shape.
func putRoute(s *Server, ictx context.Context, url, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := routingPolicyReq(ictx, "PUT", url, body, pathID(url))
	if strings.Contains(url, "/orgs/") {
		s.handlePutOrgRoutingPolicy(w, req)
	} else {
		s.handlePutTeamRoutingPolicy(w, req)
	}
	return w
}

func getRoute(s *Server, ictx context.Context, url string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := routingPolicyReq(ictx, "GET", url, "", pathID(url))
	if strings.Contains(url, "/orgs/") {
		s.handleGetOrgRoutingPolicy(w, req)
	} else {
		s.handleGetTeamRoutingPolicy(w, req)
	}
	return w
}

// pathID extracts the {id} path value: /api/{orgs|teams}/{id}/...
func pathID(url string) string {
	return strings.Split(url, "/")[3]
}

func seedRoutingPolicyWorld(t *testing.T, s *Server) {
	t.Helper()
	seedOrg(t, s, "org1", "org1")
	seedTeam(t, s, "team1", "team1")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "team2", Name: "team2", Slug: "team2", OrgID: "org1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team2: %v", err)
	}
	now := time.Now()
	for i, mb := range []identity.Membership{
		{UserID: "u-teamadmin", TeamID: "team1", Role: identity.RoleAdmin},
		{UserID: "u-teamviewer", TeamID: "team1", Role: identity.RoleViewer},
	} {
		mb.JoinedAt = now.Add(time.Duration(i) * time.Second)
		if err := s.authStore().UpsertMembership(context.Background(), mb); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	for i, om := range []identity.OrgMembership{
		{UserID: "u-orgadmin", OrgID: "org1", Role: identity.OrgRoleAdmin},
		{UserID: "u-orgmember", OrgID: "org1", Role: identity.OrgRoleMember},
	} {
		om.JoinedAt = now.Add(time.Duration(10+i) * time.Second)
		if err := s.authStore().UpsertOrgMembership(context.Background(), om); err != nil {
			t.Fatalf("seed org membership: %v", err)
		}
	}
}

// The RBAC matrix plus the write-path contract: CAS 409, the required
// routing key, the strict decode, the null-clear, and the unwired
// store. The fold-level behavior is route_resolve_test.go's suite.
func TestRoutingPolicyRoutes(t *testing.T) {
	s := newOrgTestServer(t)
	factory, _ := routingPolicyStores()
	s.routingPolicyStoreFor = factory
	seedRoutingPolicyWorld(t, s)

	teamURL := "/api/teams/team1/routing-policy"
	team2URL := "/api/teams/team2/routing-policy"
	orgURL := "/api/orgs/org1/routing-policy"
	body := `{"routing": {"pair_order": ["claw+zai_key"]}}`

	// WHO may write: the team admin on their team, the org admin on
	// their org AND on a member team (orgAdminOfTeam); nobody below
	// admin anywhere; the super admin everywhere.
	cases := []struct {
		name string
		ictx context.Context
		url  string
		want int
	}{
		{"team admin writes the team policy", asUser("u-teamadmin"), teamURL, http.StatusOK},
		{"org admin writes a member team's policy", asUser("u-orgadmin"), team2URL, http.StatusOK},
		{"org admin writes the org policy", asUser("u-orgadmin"), orgURL, http.StatusOK},
		{"org member (non-admin) refused on the team route", asUser("u-orgmember"), team2URL, http.StatusForbidden},
		{"org member (non-admin) refused on the org route", asUser("u-orgmember"), orgURL, http.StatusForbidden},
		{"team viewer refused", asUser("u-teamviewer"), teamURL, http.StatusForbidden},
		{"outsider refused on the org route", asUser("u-nobody"), orgURL, http.StatusForbidden},
		{"super admin writes", superAdminCtx(), teamURL, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := putRoute(s, tc.ictx, tc.url, body)
			if w.Code != tc.want {
				t.Fatalf("PUT %s as %s: %d, want %d", tc.url, tc.name, w.Code, tc.want)
			}
		})
	}

	// The team admin's write landed: the GET answers it, origin "db".
	w := getRoute(s, asUser("u-teamadmin"), teamURL)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	var view routingPolicyView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.Origin != "db" || view.Policy == nil || len(view.Policy.PairOrder) != 1 {
		t.Fatalf("view = %+v, want the stored policy, origin db", view)
	}

	// CAS: a stale expected_updated_at loses as a 409; the fresh one
	// lands.
	fresh := getRoute(s, asUser("u-teamadmin"), teamURL)
	var cur routingPolicyView
	_ = json.Unmarshal(fresh.Body.Bytes(), &cur)
	stale := `{"routing": {"pair_order": ["claw+openai_key"]}, "expected_updated_at": "2000-01-01T00:00:00Z"}`
	if w = putRoute(s, asUser("u-teamadmin"), teamURL, stale); w.Code != http.StatusConflict {
		t.Fatalf("stale CAS: %d, want 409", w.Code)
	}
	freshStamp := cur.UpdatedAt.Format(time.RFC3339Nano)
	freshBody := `{"routing": {"pair_order": ["claw+openai_key"]}, "expected_updated_at": "` + freshStamp + `"}`
	if w = putRoute(s, asUser("u-teamadmin"), teamURL, freshBody); w.Code != http.StatusOK {
		t.Fatalf("fresh CAS: %d, want 200", w.Code)
	}

	// A body naming no routing key is a misfire, not a keep.
	if w = putRoute(s, asUser("u-teamadmin"), teamURL, `{"expected_updated_at": "2000-01-01T00:00:00Z"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("absent routing: %d, want 400", w.Code)
	}
	// An unknown field refuses (DisallowUnknownFields).
	if w = putRoute(s, asUser("u-teamadmin"), teamURL, `{"routing": {"pair_order": [], "nonsense": 1}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d, want 400", w.Code)
	}
	// An invalid policy value refuses, naming the field.
	if w = putRoute(s, asUser("u-teamadmin"), teamURL, `{"routing": {"pair_order": ["warp+drive"]}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid pair: %d, want 400", w.Code)
	}

	// null clears: the GET answers origin "default", policy null.
	if w = putRoute(s, asUser("u-teamadmin"), teamURL, `{"routing": null}`); w.Code != http.StatusOK {
		t.Fatalf("null clear: %d, want 200", w.Code)
	}
	w = getRoute(s, asUser("u-teamadmin"), teamURL)
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if view.Origin != "default" || view.Policy != nil {
		t.Fatalf("view after clear = %+v, want origin default, policy nil", view)
	}

	// A deployment without the tenant store refuses writes loudly.
	s2 := newOrgTestServer(t)
	seedRoutingPolicyWorld(t, s2)
	if w = putRoute(s2, asUser("u-teamadmin"), teamURL, body); w.Code != http.StatusInternalServerError {
		t.Fatalf("unwired store: %d, want 500", w.Code)
	}
}

// racingStore lands a concurrent write INSIDE the handler's read→write
// window, once ARMED: its Get (the handler's prev read) returns, then —
// before the handler acts — the editor's correctly-CASsed policy lands.
// The pre-check cannot catch this race; only a stamped delete can.
type racingStore struct {
	platformcfg.Store[platformcfg.RoutingPolicyRecord]
	armed bool
	conc  platformcfg.RoutingPolicyRecord
}

func (r *racingStore) DeleteIfUnchanged(ctx context.Context, prev time.Time) (bool, error) {
	if cas, ok := r.Store.(platformcfg.CASDeleter); ok {
		return cas.DeleteIfUnchanged(ctx, prev)
	}
	return false, nil
}

func (r *racingStore) Delete(ctx context.Context) error {
	if del, ok := r.Store.(platformcfg.Deleter); ok {
		return del.Delete(ctx)
	}
	return nil
}

func (r *racingStore) Get(ctx context.Context) (*platformcfg.RoutingPolicyRecord, error) {
	rec, err := r.Store.Get(ctx)
	if err == nil && rec != nil && r.armed {
		r.armed = false
		if cas, ok := r.Store.(platformcfg.CASStore[platformcfg.RoutingPolicyRecord]); ok {
			if _, err := cas.PutIfUnchanged(ctx, r.conc, rec.UpdatedAt); err != nil {
				return rec, err
			}
		}
	}
	return rec, err
}

// The clear races a concurrent editor like the replace does: a null-write
// racing a just-landed policy must lose as a 409 and leave the editor's
// policy in place — never destroy it behind a 200. (The review probe:
// two admins, one clicks Clear while the other Saves, the write landing
// inside the handler's read→delete window.)
func TestRoutingPolicyRoutes_ClearRacesAConcurrentWrite(t *testing.T) {
	s := newOrgTestServer(t)
	factory, _ := routingPolicyStores()
	teamStore := factory(platformcfg.TeamRoutingPolicyID("team1"))
	racing := &racingStore{
		Store: teamStore,
		conc:  platformcfg.RoutingPolicyRecord{Policy: &llmroute.Policy{PairOrder: []string{"codex+chatgpt_forfait"}}},
	}
	s.routingPolicyStoreFor = func(docID string) platformcfg.Store[platformcfg.RoutingPolicyRecord] {
		if docID == platformcfg.TeamRoutingPolicyID("team1") {
			return racing
		}
		return factory(docID)
	}
	seedRoutingPolicyWorld(t, s)

	teamURL := "/api/teams/team1/routing-policy"
	body := `{"routing": {"pair_order": ["claw+zai_key"]}}`
	if w := putRoute(s, asUser("u-teamadmin"), teamURL, body); w.Code != http.StatusOK {
		t.Fatalf("seed write: %d", w.Code)
	}

	// Arm the race: the NEXT read of the team store (the clear's own
	// prev read) lands the concurrent editor's policy. No
	// expected_updated_at on the clear — the client trusts the server's
	// own read; only a stamped delete can lose this race loudly.
	racing.armed = true
	w := putRoute(s, asUser("u-teamadmin"), teamURL, `{"routing": null}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("clear racing a just-landed policy: %d, want 409", w.Code)
	}
	cur, err := teamStore.Get(context.Background())
	if err != nil || cur == nil || len(cur.Policy.PairOrder) != 1 || cur.Policy.PairOrder[0] != "codex+chatgpt_forfait" {
		t.Fatalf("policy after the raced clear = %+v err=%v, want the concurrent editor's", cur, err)
	}
}
