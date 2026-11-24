package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/knowledge"
)

type quotaFailureStore struct {
	knowledge.MemoryStore
	err    error
	quotas map[string]int64
}

func (s *quotaFailureStore) SetTenantQuota(_ context.Context, team string, quota int64) error {
	if s.err != nil {
		return s.err
	}
	s.quotas[team] = quota
	return nil
}

func TestAdminQuotaFailureNeverAcknowledgesEnforcement(t *testing.T) {
	for _, failure := range []string{"team-read", "quota-write"} {
		t.Run(failure, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			mem := &quotaFailureStore{quotas: map[string]int64{}}
			f.s.memStore = mem
			if failure == "team-read" {
				f.ids.fail("ListTeamsByOrg", 0)
			} else {
				mem.err = errClusterUnreachable
			}
			request := func() *httptest.ResponseRecorder {
				r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, IsSuperAdmin: true}), "PATCH", "/api/admin/orgs/o1", `{"memory_quota_bytes":12345}`, "o1")
				w := httptest.NewRecorder()
				f.s.handleAdminUpdateOrg(w, r)
				return w
			}
			w := request()
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("quota enforcement failure acknowledged as success: %d %s", w.Code, w.Body.String())
			}
			assertIdentityUnavailable(t, w, f.logs, "quota propagation")
			f.ids.disarm()
			mem.err = nil
			w = request()
			if w.Code != http.StatusOK || mem.quotas["ta"] != 12345 {
				t.Fatalf("retry does not repair enforcement: %d %s quotas=%v", w.Code, w.Body.String(), mem.quotas)
			}
		})
	}
}

type corruptOrgReadStore struct{ identity.Store }

func (s corruptOrgReadStore) GetOrgsByIDs(ctx context.Context, ids []string) (map[string]identity.Org, error) {
	rows, err := s.Store.GetOrgsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	delete(rows, "o1")
	return rows, &identity.BulkDecodeError{Operation: "decode orgs", Failures: map[string]error{"o1": errClusterUnreachable}}
}

func TestMeKeepsHealthyOrgsAndMarksCorruptMembership(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.ids.Store = corruptOrgReadStore{f.ids.Store}
	r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID}), "GET", "/api/auth/me", "", "")
	w := httptest.NewRecorder()
	f.s.handleMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("corrupt org starves readable siblings: %d %s", w.Code, w.Body.String())
	}
	var body AuthMeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Orgs) != 2 {
		t.Fatalf("lost org membership: %s", w.Body.String())
	}
	var healthy, corrupt bool
	for _, org := range body.Orgs {
		if org.OrgID == "o1" {
			corrupt = org.Unavailable && org.Teams != nil && len(org.Teams) == 0 && org.OrgRole == "" && org.OrgName == ""
		} else {
			healthy = !org.Unavailable && len(org.Teams) > 0
		}
	}
	if !healthy || !corrupt {
		t.Fatalf("partial identity is not explicit or usable: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), internalHost) || !strings.Contains(f.logs.String(), internalHost) || !strings.Contains(f.logs.String(), `document "o1"`) {
		t.Fatalf("corruption details must name the document only in logs: body=%s logs=%s", w.Body.String(), f.logs.String())
	}
}

type oidcReadFailureStore struct{ identity.Store }

func (s oidcReadFailureStore) ListOIDCLinksByUser(context.Context, string) ([]identity.OIDCLink, error) {
	return nil, errClusterUnreachable
}

// Each identity route family reaches its real handler and failing store read.
// Together with the helper/status matrix this covers both raw-error sites and
// the auth helper that already hides its cause.
func TestServerErrorRouteFamiliesNeverExposeStoreDetails(t *testing.T) {
	type handler func(*Server, http.ResponseWriter, *http.Request)
	for _, tc := range []struct {
		family, method, body, id, read string
		handle                         handler
	}{
		{"auth_sessions", "POST", `{"email":"u1@example.com","password":"password-forever-9"}`, "", "GetUserByEmail", (*Server).handleLogin},
		{"auth_teams", "GET", "", "", "ListMembershipsByUser", (*Server).handleListTeams},
		{"team_lifecycle", "GET", "", "ta", "GetTeam", (*Server).handleGetTeam},
		{"orgs", "GET", "", "o1", "GetOrg", (*Server).handleGetOrg},
		{"admin_orgs", "GET", "", "o1", "GetOrg", (*Server).handleAdminGetOrg},
		{"auth_admin", "PATCH", `{}`, "u1", "GetUser", (*Server).handleAdminUpdateUser},
		{"admin_teams", "PUT", `{"runner_pool":""}`, "ta", "GetTeam", (*Server).handleAdminSetTeamRunnerPool},
		{"org_credentials", "GET", "", "o1", "GetOrg", (*Server).handleGetOrgCredentialAudience},
		{"auth_oidc", "GET", "", "", "", (*Server).handleListMySSOLinks},
		{"password", "POST", `{"current_password":"password-forever-9","new_password":"new-password-forever-9"}`, "", "GetUser", (*Server).handleChangeMyPassword},
		{"admin_users", "GET", "", "u1", "GetUser", (*Server).handleAdminGetUser},
	} {
		t.Run(tc.family, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			if tc.read == "" {
				f.ids.Store = oidcReadFailureStore{f.ids.Store}
			} else {
				f.ids.fail(tc.read, 0)
			}
			r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, IsSuperAdmin: true, OrgID: "o1", OrgRole: identity.OrgRoleAdmin, Credential: auth.CredentialAccessToken}), tc.method, "/api/"+tc.family, tc.body, tc.id)
			w := httptest.NewRecorder()
			errtrack.HTTPErrorMiddleware(f.s.logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tc.handle(f.s, w, r) })).ServeHTTP(w, r)
			assertIdentityUnavailable(t, w, f.logs, tc.family)
			var body map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body["request_id"] != w.Header().Get("X-Request-ID") || !strings.Contains(f.logs.String(), body["request_id"]) {
				t.Fatalf("public error cannot be correlated with its cause: body=%s log=%s", w.Body.String(), f.logs.String())
			}
		})
	}
}

func TestHTTPErrorHelpersShareTheFailureBoundary(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/api/error-test", nil)
	for _, status := range []int{400, 401, 403, 404, 409, 422, 429, 500, 502, 503, 504, 599} {
		for _, helper := range []string{"plain", "origin", "coded", "structured", "status", "dispatcher"} {
			w := httptest.NewRecorder()
			switch helper {
			case "plain":
				httpError(w, status, "%s", internalHost)
			case "origin":
				s.httpErrorFor(w, r, status, "%s", internalHost)
			case "coded":
				s.httpErrorCode(w, r, status, "stable_code", "%s", internalHost)
			case "structured":
				s.writeJSONError(w, r, status, map[string]any{"error": internalHost, "error_code": "stable_code", "retryable": true, "nested": map[string]any{"diagnostic": internalHost}})
			case "status":
				writeJSONStatus(w, status, map[string]any{"error": internalHost, "error_code": "stable_code"})
			case "dispatcher":
				dispatcher.WriteErr(w, status, fmt.Errorf("driver: %s", internalHost))
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != status {
				t.Fatalf("status changed: %d != %d", w.Code, status)
			}
			if status >= 500 {
				id, _ := body["request_id"].(string)
				if strings.Contains(w.Body.String(), internalHost) || body["error"] != identityUnavailable || id == "" {
					t.Fatalf("%s %d leaks its cause: %s", helper, status, w.Body.String())
				}
			} else if !strings.Contains(fmt.Sprint(body["error"]), internalHost) || body["request_id"] != nil {
				t.Fatalf("%s %d lost a client's refusal: %s", helper, status, w.Body.String())
			}
			if (helper == "coded" || helper == "structured" || helper == "status") && body["error_code"] != "stable_code" {
				t.Fatal("lost machine-readable code")
			}
			if helper == "structured" && body["retryable"] != true {
				t.Fatal("lost retryability")
			}
		}
	}
}

type unreadableBoard struct{ native.BoardStore }

func (unreadableBoard) Board() (*native.Board, error) { return nil, errClusterUnreachable }

func TestNativeBoardStoreFailureUsesTheSameBoundary(t *testing.T) {
	mux := http.NewServeMux()
	api := native.BoardAPI{Resolve: func(*http.Request) (native.BoardStore, error) { return unreadableBoard{}, nil }}
	api.RegisterRoutesWithMiddleware(mux, "/api/native", nil)
	w := newErrorRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/native/board", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), internalHost) {
		t.Fatalf("native board bypasses the failure boundary: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Diagnostic(t), internalHost) {
		t.Fatalf("native board dropped the internal cause: %s", w.logs.String())
	}
}
