package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The journey an operator makes when an account signs in and sees nothing:
// find it, read where it came from, place it, and watch it gain access.
//
// It only exists over real HTTP. The pieces live apart — requireSuperAdmin,
// buildAdminUserDetail, handlePutOrgMember, handlePutTeamMember, canViewTeam
// — and the property that matters is their COMPOSITION: that the org
// membership is genuinely a prerequisite, that placing the account genuinely
// opens the team, and that the account file genuinely reports what was
// granted. Each of those is invisible to a unit test of any one handler.

type placementE2E struct {
	s      *Server
	hs     *httptest.Server
	signer *auth.JWTSigner
}

func newPlacementE2E(t *testing.T) *placementE2E {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupInviteOnly,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	s := New(Config{
		WorkDir:                 t.TempDir(),
		Bind:                    "127.0.0.1",
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
	}, iterlog.New(iterlog.LevelError, nil))

	ctx := context.Background()
	seedOrg(t, s, "o1", "sdpc")
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t1", OrgID: "o1", Name: "PIC", Slug: "pic", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	mk := func(id, email string, withPassword bool) {
		t.Helper()
		u := identity.User{ID: id, Email: email, Status: identity.UserStatusActive, CreatedAt: time.Now().UTC()}
		if withPassword {
			u.PasswordHash = "$2a$10$notarealhash"
		}
		if _, err := s.authStore().CreateUser(ctx, u); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("root", "root@example.org", true)
	// The account the console exists for: admitted by GitHub SSO outside the
	// allow-list, so active, no password, no org, no team.
	mk("sub1", "brahim@externes.example.org", false)
	// A neighbour whose email shares a SUBSTRING but not the PREFIX, so the
	// search assertions below can tell the two matching rules apart.
	mk("mid1", "xx-brahim@example.org", true)
	// An ordinary org admin, to check the super-admin console is not open
	// to them.
	mk("orgadmin", "orgadmin@example.org", true)
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "orgadmin", OrgID: "o1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed org admin: %v", err)
	}
	if err := s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider: "github", ProviderUserID: "7654321", UserID: "sub1",
		Email: "brahim@externes.example.org", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed oidc link: %v", err)
	}

	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return &placementE2E{s: s, hs: hs, signer: signer}
}

func (p *placementE2E) jwt(t *testing.T, id auth.Identity) string {
	t.Helper()
	tok, _, err := p.signer.IssueAccess(id)
	if err != nil {
		t.Fatalf("issue access for %s: %v", id.UserID, err)
	}
	return tok
}

func (p *placementE2E) call(t *testing.T, method, path, bearer, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, p.hs.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (p *placementE2E) detail(t *testing.T, bearer, userID string) adminUserDetailView {
	t.Helper()
	status, body := p.call(t, http.MethodGet, "/api/admin/users/"+userID, bearer, "")
	if status != http.StatusOK {
		t.Fatalf("GET account file = %d body=%s, want 200", status, body)
	}
	var v adminUserDetailView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode account file: %v (%s)", err, body)
	}
	return v
}

func TestAdminPlacement_FindReadPlaceAndGainAccess(t *testing.T) {
	p := newPlacementE2E(t)
	root := p.jwt(t, auth.Identity{UserID: "root", Email: "root@example.org", IsSuperAdmin: true})
	orgAdmin := p.jwt(t, auth.Identity{
		UserID: "orgadmin", Email: "orgadmin@example.org", OrgID: "o1", OrgRole: identity.OrgRoleAdmin,
	})
	sub := p.jwt(t, auth.Identity{UserID: "sub1", Email: "brahim@externes.example.org"})

	// The boundary, before: the account can authenticate and reaches nothing.
	if status, body := p.call(t, http.MethodGet, "/api/teams/t1/members", sub, ""); status != http.StatusForbidden {
		t.Fatalf("submitter read before placement = %d body=%s, want 403", status, body)
	}

	t.Run("the console is super-admin only", func(t *testing.T) {
		// An ORG admin manages this org and still may not read the platform's
		// account files: they are cross-tenant by nature.
		if status, _ := p.call(t, http.MethodGet, "/api/admin/users/sub1", orgAdmin, ""); status != http.StatusForbidden {
			t.Fatalf("org admin account file = %d, want 403", status)
		}
		if status, _ := p.call(t, http.MethodGet, "/api/admin/users?q=brahim", orgAdmin, ""); status != http.StatusForbidden {
			t.Fatalf("org admin search = %d, want 403", status)
		}
	})

	t.Run("search finds it by email PREFIX, not by substring", func(t *testing.T) {
		status, body := p.call(t, http.MethodGet, "/api/admin/users?q=brahim", root, "")
		if status != http.StatusOK {
			t.Fatalf("search = %d body=%s, want 200", status, body)
		}
		var got struct {
			Users []UserView `json:"users"`
			Query string     `json:"query"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode search: %v", err)
		}
		if got.Query != "brahim" {
			t.Fatalf("query echo = %q, want brahim", got.Query)
		}
		// "xx-brahim@example.org" CONTAINS the query and must not appear;
		// without that row the assertion could not tell the two rules apart.
		if len(got.Users) != 1 || got.Users[0].ID != "sub1" {
			t.Fatalf("search returned %+v, want only sub1", got.Users)
		}
		// A regex metacharacter is matched literally, not as a pattern.
		_, body = p.call(t, http.MethodGet, "/api/admin/users?q=.%2A", root, "")
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode search: %v", err)
		}
		if len(got.Users) != 0 {
			t.Fatalf(`q=".*" returned %d users, want 0 — the query reached the store unescaped`, len(got.Users))
		}
	})

	t.Run("the account file names the cause", func(t *testing.T) {
		got := p.detail(t, root, "sub1")
		if got.HasPassword {
			t.Fatalf("want no password: %+v", got)
		}
		if len(got.Orgs) != 0 || len(got.Teams) != 0 {
			t.Fatalf("want an empty roster: orgs=%+v teams=%+v", got.Orgs, got.Teams)
		}
		if len(got.SSOLinks) != 1 || got.SSOLinks[0].Provider != "github" || got.SSOLinks[0].Subject != "7654321" {
			t.Fatalf("want the github link with its subject: %+v", got.SSOLinks)
		}
		if got.User.Status != string(identity.UserStatusActive) {
			t.Fatalf("want an ACTIVE account — that is what makes it confusing: %+v", got.User)
		}
		if got.User.LastLoginAt != "" {
			t.Fatalf("this account never completed a sign-in: %+v", got.User)
		}
	})

	t.Run("a team grant is refused before the org membership", func(t *testing.T) {
		// The UI stops offering this, but the guard is on the server and
		// stays reachable — a refusal does not disappear because a picker
		// became polite.
		status, body := p.call(t, http.MethodPut, "/api/teams/t1/members/sub1", root, `{"role":"member"}`)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("team grant without org membership = %d body=%s, want 422", status, body)
		}
		if !bytes.Contains(body, []byte("organization")) {
			t.Fatalf("the 422 must say WHY: %s", body)
		}
		// And nothing was created behind the refusal.
		if _, err := p.s.authStore().GetMembership(context.Background(), "sub1", "t1"); err == nil {
			t.Fatalf("a refused grant must not exist")
		}
	})

	t.Run("placing it org-first opens the team", func(t *testing.T) {
		if status, body := p.call(t, http.MethodPut, "/api/orgs/o1/members/sub1", root, `{"role":"member"}`); status != http.StatusOK {
			t.Fatalf("org placement = %d body=%s, want 200", status, body)
		}
		if status, body := p.call(t, http.MethodPut, "/api/teams/t1/members/sub1", root, `{"role":"member"}`); status != http.StatusOK {
			t.Fatalf("team placement = %d body=%s, want 200", status, body)
		}

		// The observable proof — minted through SwitchTeam, NOT forged.
		//
		// Forging the claims here would measure the forgery: canViewTeam
		// short-circuits on `id.TeamID == teamID && id.Role.AtLeast(viewer)`
		// and never reaches the store, so the 403 at the top of this test and
		// a 200 here would differ only by which token was handed over — the
		// placement would not be in the causal chain. SwitchTeam reads the
		// membership, so it refuses outright when the grant is missing, which
		// is what makes this assertion redden if either PUT fails to persist.
		_, access, _, err := p.s.authSvc.SwitchTeam(context.Background(), "sub1", "t1")
		if err != nil {
			t.Fatalf("SwitchTeam after placement: %v — the grant did not persist", err)
		}
		if status, body := p.call(t, http.MethodGet, "/api/teams/t1/members", access, ""); status != http.StatusOK {
			t.Fatalf("read after placement = %d body=%s, want 200", status, body)
		}
	})

	t.Run("the file now reports both grants, with their real roles", func(t *testing.T) {
		got := p.detail(t, root, "sub1")
		if len(got.Orgs) != 1 || got.Orgs[0].OrgID != "o1" || got.Orgs[0].Role != "member" {
			t.Fatalf("org row = %+v", got.Orgs)
		}
		if len(got.Teams) != 1 || got.Teams[0].TeamID != "t1" || got.Teams[0].Role != "member" {
			t.Fatalf("team row = %+v", got.Teams)
		}
		if got.Teams[0].OrgName != "o1" || got.Teams[0].OrphanGrant {
			t.Fatalf("the team row must name its org and not read as an orphan: %+v", got.Teams[0])
		}
	})

	t.Run("placement is idempotent and re-running it sets the role", func(t *testing.T) {
		if status, _ := p.call(t, http.MethodPut, "/api/teams/t1/members/sub1", root, `{"role":"admin"}`); status != http.StatusOK {
			t.Fatalf("re-placement = %d, want 200", status)
		}
		got := p.detail(t, root, "sub1")
		if len(got.Teams) != 1 || got.Teams[0].Role != "admin" {
			t.Fatalf("want ONE row at the new role, got %+v", got.Teams)
		}
	})
}

// TestAdminPlacement_OrgIsReadableWithoutMembership covers the other half:
// /orgs/:id used to be a dead screen for a super-admin, because the page
// resolved its subject from the caller's own tree while every API behind it
// would have answered.
func TestAdminPlacement_OrgIsReadableWithoutMembership(t *testing.T) {
	p := newPlacementE2E(t)
	root := p.jwt(t, auth.Identity{UserID: "root", Email: "root@example.org", IsSuperAdmin: true})
	stranger := p.jwt(t, auth.Identity{UserID: "sub1", Email: "brahim@externes.example.org"})

	// The super-admin holds NO membership in o1 — that is the whole point.
	if _, err := p.s.authStore().GetOrgMembership(context.Background(), "root", "o1"); err == nil {
		t.Fatalf("premise broken: root is a member of o1, so this proves nothing")
	}

	status, body := p.call(t, http.MethodGet, "/api/orgs/o1", root, "")
	if status != http.StatusOK {
		t.Fatalf("super-admin org read = %d body=%s, want 200", status, body)
	}
	var got orgView
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode org: %v", err)
	}
	if got.ID != "o1" || got.Slug != "sdpc" {
		t.Fatalf("org view = %+v", got)
	}

	// And it is not a hole: someone with no claim on the org is refused.
	if status, _ := p.call(t, http.MethodGet, "/api/orgs/o1", stranger, ""); status != http.StatusForbidden {
		t.Fatalf("stranger org read = %d, want 403", status)
	}
}
