package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestHandleListTeams_BulkLookup covers the batched team resolution:
// rows keep the membership order (JoinedAt asc), and a membership whose
// team no longer exists is skipped — same as the old per-row GetTeam
// ErrNotFound path.
func TestHandleListTeams_BulkLookup(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, tm := range []identity.Team{
		{ID: "t1", Name: "Alpha", Slug: "alpha", CreatedAt: now},
		{ID: "t2", Name: "Beta", Slug: "beta", Personal: true, CreatedAt: now.Add(time.Second)},
	} {
		if _, err := s.authStore().CreateTeam(ctx, tm); err != nil {
			t.Fatalf("seed team %s: %v", tm.ID, err)
		}
	}
	// Membership order is JoinedAt asc: t2 first, then a dangling team,
	// then t1.
	for i, mb := range []identity.Membership{
		{UserID: "u1", TeamID: "t2", Role: identity.RoleViewer},
		{UserID: "u1", TeamID: "t-gone", Role: identity.RoleMember},
		{UserID: "u1", TeamID: "t1", Role: identity.RoleAdmin},
	} {
		mb.JoinedAt = now.Add(time.Duration(i) * time.Second)
		if err := s.authStore().UpsertMembership(ctx, mb); err != nil {
			t.Fatalf("seed membership %s: %v", mb.TeamID, err)
		}
	}

	idCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "u1"})
	w := httptest.NewRecorder()
	s.handleListTeams(w, httptest.NewRequest("GET", "/api/teams", nil).WithContext(idCtx))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Teams []MembershipView `json:"teams"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Teams) != 2 {
		t.Fatalf("got %d teams, want 2 (dangling membership skipped): %+v", len(resp.Teams), resp.Teams)
	}
	if resp.Teams[0].TeamID != "t2" || resp.Teams[1].TeamID != "t1" {
		t.Fatalf("membership order lost: %+v", resp.Teams)
	}
	if resp.Teams[0].TeamName != "Beta" || !resp.Teams[0].Personal || resp.Teams[0].Role != "viewer" {
		t.Fatalf("t2 view mismatch: %+v", resp.Teams[0])
	}
	if resp.Teams[1].TeamSlug != "alpha" || resp.Teams[1].Role != "admin" {
		t.Fatalf("t1 view mismatch: %+v", resp.Teams[1])
	}
}

// TestHandleListTeamMembers_BulkLookup covers the batched user
// resolution: rows keep the membership order, and a member whose user
// record is gone still renders with user_id + role and empty
// email/name — same as the old swallowed per-row GetUser error.
func TestHandleListTeamMembers_BulkLookup(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "u1", Email: "one@example.com", Name: "One",
		Status: identity.UserStatusActive, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedTeam(t, s, "t1", "alpha")
	for i, mb := range []identity.Membership{
		{UserID: "u1", TeamID: "t1", Role: identity.RoleOwner},
		{UserID: "ghost", TeamID: "t1", Role: identity.RoleMember},
	} {
		mb.JoinedAt = now.Add(time.Duration(i) * time.Second)
		if err := s.authStore().UpsertMembership(ctx, mb); err != nil {
			t.Fatalf("seed membership %s: %v", mb.UserID, err)
		}
	}

	idCtx := auth.WithIdentity(ctx, auth.Identity{UserID: "u1", TeamID: "t1", Role: identity.RoleOwner})
	r := httptest.NewRequest("GET", "/api/teams/t1/members", nil).WithContext(idCtx)
	r.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	s.handleListTeamMembers(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Members []struct {
			UserID string `json:"user_id"`
			Email  string `json:"email"`
			Name   string `json:"name"`
			Role   string `json:"role"`
		} `json:"members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Members) != 2 {
		t.Fatalf("got %d members, want 2 (missing user keeps its row): %+v", len(resp.Members), resp.Members)
	}
	if m := resp.Members[0]; m.UserID != "u1" || m.Email != "one@example.com" || m.Name != "One" || m.Role != "owner" {
		t.Fatalf("u1 row mismatch: %+v", m)
	}
	if m := resp.Members[1]; m.UserID != "ghost" || m.Email != "" || m.Name != "" || m.Role != "member" {
		t.Fatalf("ghost row must keep id+role with empty user fields: %+v", m)
	}
}
