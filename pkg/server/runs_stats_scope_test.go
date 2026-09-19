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
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestHandleRunsStatsHonoursTeamScope is the SITE test for #1419: the
// helper (TestResolveTenantScope) proves the seam, this one proves the
// handler is WIRED to it. Mutation: delete the resolveTenantScope call in
// handleRunsStats → the unauthorised row's 403 disappears → red.
func TestHandleRunsStatsHonoursTeamScope(t *testing.T) {
	s := newOrgTestServer(t)
	seedTeam(t, s, "team-A", "team-a")
	seedTeam(t, s, "team-C", "team-c")
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// Wire a real run service so the handler reaches its aggregation.
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs))

	tests := []struct {
		name       string
		id         auth.Identity
		queryTeam  string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "unauthorised cross-team scope is refused naming the parameter",
			id:         auth.Identity{UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember},
			queryTeam:  "team-C",
			wantStatus: http.StatusForbidden,
			wantBody:   "team_id=team-C",
		},
		{
			name:       "super-admin cross-team scope reaches the aggregation",
			id:         auth.Identity{UserID: "u-admin", IsSuperAdmin: true},
			queryTeam:  "team-C",
			wantStatus: http.StatusOK,
		},
		{
			name:       "no scope parameter answers for the active team",
			id:         auth.Identity{UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember},
			queryTeam:  "",
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.WithIdentity(context.Background(), tc.id)
			path := "/api/v1/runs/stats"
			if tc.queryTeam != "" {
				path += "?team_id=" + tc.queryTeam
			}
			r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
			w := httptest.NewRecorder()
			s.handleRunsStats(w, r)

			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s; want %d", w.Code, w.Body.String(), tc.wantStatus)
			}
			if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Errorf("body %q missing %q — the refusal must NAME the parameter (#1419: never silent)", w.Body.String(), tc.wantBody)
			}
			if tc.wantStatus == http.StatusOK {
				var out StatsResponse
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode stats body: %v (%q)", err, w.Body.String())
				}
			}
		})
	}
}
