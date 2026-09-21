package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// spyRunStore records the ctx the handler's list actually reached — the
// filesystem store does not tenant-filter, so the ONLY observable proof
// that resolveTenantScope's re-stamped ctx (not r.Context()) reached the
// store is this capture. #1510 Rb5f386: the pre-fix OK rows asserted
// status + decodable body only, and a revert of handleRunsStats to
// r.Context() (keeping the resolveTenantScope call for the 403) survived
// them — the silent-wrong-tenant regression the ticket was about.
type spyRunStore struct {
	store.RunStore
	mu      sync.Mutex
	lastCtx context.Context
}

func (s *spyRunStore) ListRuns(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	s.lastCtx = ctx
	s.mu.Unlock()
	return s.RunStore.ListRuns(ctx)
}

func (s *spyRunStore) lastSeenCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCtx
}

// TestHandleRunsStatsHonoursTeamScope is the SITE test for #1419: the
// helper (TestResolveTenantScope) proves the seam, this one proves the
// handler is WIRED to it. Two mutations the test reddens:
//
//   - delete the resolveTenantScope call in handleRunsStats → the
//     unauthorised row's 403 disappears;
//   - revert to r.Context() while KEEPING the scope call → the 403 row
//     stays green but the spy sees a tenant-less ctx on the scoped OK
//     rows — the silent-wrong-tenant shape.
func TestHandleRunsStatsHonoursTeamScope(t *testing.T) {
	s := newOrgTestServer(t)
	seedTeam(t, s, "team-A", "team-a")
	seedTeam(t, s, "team-C", "team-c")
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// Wire a real run service so the handler reaches its aggregation, with
	// a spy capturing the ctx that reaches the store's listing.
	underlying, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	spy := &spyRunStore{RunStore: underlying}
	s.runs = newTestRunviewService(t, "", runview.WithStore(spy))

	tests := []struct {
		name          string
		id            auth.Identity
		queryTeam     string
		wantStatus    int
		wantBody      string
		wantCtxTenant string // tenant the store's ListRuns must have seen ("" = refused row, no list happens)
	}{
		{
			name:       "unauthorised cross-team scope is refused naming the parameter",
			id:         auth.Identity{UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember},
			queryTeam:  "team-C",
			wantStatus: http.StatusForbidden,
			wantBody:   "team_id=team-C",
		},
		{
			name:          "super-admin cross-team scope reaches the aggregation",
			id:            auth.Identity{UserID: "u-admin", IsSuperAdmin: true},
			queryTeam:     "team-C",
			wantStatus:    http.StatusOK,
			wantCtxTenant: "team-C",
		},
		{
			name:          "no scope parameter answers for the active team",
			id:            auth.Identity{UserID: "u-member", TeamID: "team-A", Role: identity.RoleMember},
			queryTeam:     "",
			wantStatus:    http.StatusOK,
			wantCtxTenant: "team-A",
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
				if tc.wantCtxTenant != "" {
					gotTenant, ok := store.TenantFromContext(spy.lastSeenCtx())
					if !ok {
						t.Fatalf("the store's ListRuns saw a ctx with NO tenant — resolveTenantScope's re-stamped ctx never reached the store (the #1419 silent-wrong-tenant shape)")
					}
					if gotTenant != tc.wantCtxTenant {
						t.Errorf("store saw tenant %q; want %q", gotTenant, tc.wantCtxTenant)
					}
				}
			}
		})
	}
}
