package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResolveTenantScope is the property test for #1419's fix. Every route
// that opts into cross-tenant reads goes through resolveTenantScope; the
// table below asserts the four verdicts the helper must produce, so a new
// route inherits the property by using the helper — the pre-fix "accept-
// and-drop" shape (byte-identical answers across three tenants because the
// parameter went to net/http.ServeMux and never reached the handler) is
// structurally unreachable through this seam.
//
// The verdicts are named after the class of failure they exclude:
//
//   - active-team-default: no ?team_id= AND no header → active team is used.
//     Failure mode: a regression that starts requiring the header.
//   - same-team-explicit: ?team_id=<active> → active team is used, no elevation.
//     Failure mode: a regression that always overrides even for a no-op.
//   - super-admin-override: cross-tenant team_id, super-admin → target used.
//     Failure mode: 403 for a user who should read every tenant.
//   - member-override: cross-tenant team_id, direct membership → target used.
//     Failure mode: 403 for a user who legitimately reads a second team.
//   - non-member-refusal: cross-tenant team_id, no membership → 403 with
//     the parameter NAMED in the body. Failure mode: silent drop back to
//     active — the #1419 bug — or an untyped error that hides the cause.
func TestResolveTenantScope(t *testing.T) {
	s := newOrgTestServer(t)
	// Seed two teams and a user with membership in only one of them.
	seedTeam(t, s, "team-A", "team-a")
	seedTeam(t, s, "team-B", "team-b")
	seedTeam(t, s, "team-C", "team-c")
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed A member: %v", err)
	}
	// u-member is also a member of team-B (a legitimate cross-team read).
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "u-member", TeamID: "team-B", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed B member: %v", err)
	}
	// u-member is NOT a member of team-C.

	admin := auth.Identity{UserID: "u-admin", IsSuperAdmin: true}
	member := auth.Identity{UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember}

	tests := []struct {
		name        string
		id          auth.Identity
		queryTeam   string
		headerTeam  string
		wantTeamID  string
		wantStatus  int // 0 → helper returns ok=true
		wantMessage string
	}{
		{"active-team-default", member, "", "", "team-A", 0, ""},
		{"same-team-explicit", member, "team-A", "", "team-A", 0, ""},
		{"super-admin-override-query", admin, "team-C", "", "team-C", 0, ""},
		{"super-admin-override-header", admin, "", "team-C", "team-C", 0, ""},
		{"member-override-authorised", member, "team-B", "", "team-B", 0, ""},
		{"non-member-refusal", member, "team-C", "", "", 403, "team_id=team-C"},
		{"query-wins-over-header", admin, "team-C", "team-A", "team-C", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.WithIdentity(context.Background(), tc.id)
			r := httptest.NewRequest("GET", "/api/v1/runs/stats", nil)
			r = r.WithContext(ctx)
			if tc.queryTeam != "" {
				q := r.URL.Query()
				q.Set("team_id", tc.queryTeam)
				r.URL.RawQuery = q.Encode()
			}
			if tc.headerTeam != "" {
				r.Header.Set(tenantScopeHeader, tc.headerTeam)
			}
			w := httptest.NewRecorder()

			got, gotCtx, ok := s.resolveTenantScope(w, r)
			if tc.wantStatus != 0 {
				if ok {
					t.Fatalf("expected refusal (ok=false), got ok=true team=%s", got)
				}
				if w.Code != tc.wantStatus {
					t.Fatalf("status=%d body=%q; want %d", w.Code, w.Body.String(), tc.wantStatus)
				}
				if tc.wantMessage != "" && !containsBody(w.Body.String(), tc.wantMessage) {
					t.Errorf("body %q missing %q — the refusal must NAME the parameter (#1419: never silent)", w.Body.String(), tc.wantMessage)
				}
				return
			}
			if !ok {
				t.Fatalf("expected ok=true; got ok=false, status=%d body=%q", w.Code, w.Body.String())
			}
			if got != tc.wantTeamID {
				t.Errorf("teamID=%q; want %q", got, tc.wantTeamID)
			}
			// The returned ctx must carry the resolved tenant. A ctx that
			// still holds the JWT's active team would send the store on a
			// query against the wrong tenant — this line falsifies exactly
			// the pre-#1419 defect.
			gotTenant, hasTenant := store.TenantFromContext(gotCtx)
			if !hasTenant {
				t.Errorf("resolved ctx has no tenant stamped — the store would fall back to WithoutTenantFilter")
			}
			if gotTenant != tc.wantTeamID {
				t.Errorf("ctx tenant=%q; want %q — the resolved ctx must scope reads to the target team", gotTenant, tc.wantTeamID)
			}
		})
	}
}

// containsBody is a small readability helper: strings.Contains inlined so the
// test's assertion reads as one line per row.
func containsBody(body, want string) bool {
	if body == "" || want == "" {
		return want == ""
	}
	for i := 0; i+len(want) <= len(body); i++ {
		if body[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
