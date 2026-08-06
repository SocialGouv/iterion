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
	"net/url"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// An invitation is how a person GAINS access to a team's resources, so the
// feature is a permission boundary end to end: only an admin may issue one,
// the token is looked up anonymously (the invitee is not signed in yet), and
// accepting must actually create the membership — the observable proof being
// that an outsider who was refused the team's endpoints before is served
// after, with the role they were invited at.
//
// The three halves live in different places (canManageTeam, the public
// lookup route, authSvc.AcceptInvitationForExistingUser) and only compose
// over real HTTP, which is what this test drives.

type inviteE2E struct {
	s      *Server
	hs     *httptest.Server
	signer *auth.JWTSigner
}

func newInviteE2E(t *testing.T) *inviteE2E {
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
	seedTeam(t, s, "t1", "acme")
	mkUser := func(id, email string) {
		t.Helper()
		if _, err := s.authStore().CreateUser(ctx, identity.User{
			ID: id, Email: email, Status: identity.UserStatusActive,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mkUser("admin", "admin@x")
	mkUser("member", "member@x")
	mkUser("outsider", "outsider@x")
	for _, m := range []identity.Membership{
		{UserID: "admin", TeamID: "t1", Role: identity.RoleAdmin},
		{UserID: "member", TeamID: "t1", Role: identity.RoleMember},
	} {
		if err := s.authStore().UpsertMembership(ctx, m); err != nil {
			t.Fatalf("membership %s: %v", m.UserID, err)
		}
	}
	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return &inviteE2E{s: s, hs: hs, signer: signer}
}

// jwt issues a session token for one of the seeded users.
func (i *inviteE2E) jwt(t *testing.T, userID, teamID string, role identity.Role) string {
	t.Helper()
	tok, _, err := i.signer.IssueAccess(auth.Identity{
		UserID: userID, Email: userID + "@x", TeamID: teamID, Role: role,
	})
	if err != nil {
		t.Fatalf("issue access for %s: %v", userID, err)
	}
	return tok
}

func (i *inviteE2E) call(t *testing.T, method, path, bearer, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, i.hs.URL+path, rdr)
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

func TestInvitations_AdminIssuesAnonymousLookupAcceptGrantsMembership(t *testing.T) {
	i := newInviteE2E(t)
	adminJWT := i.jwt(t, "admin", "t1", identity.RoleAdmin)
	memberJWT := i.jwt(t, "member", "t1", identity.RoleMember)
	outsiderJWT := i.jwt(t, "outsider", "", "")

	// The boundary, before: the outsider cannot read the team.
	if status, body := i.call(t, http.MethodGet, "/api/teams/t1/members", outsiderJWT, ""); status != http.StatusForbidden {
		t.Fatalf("outsider read before invite = %d body=%s, want 403", status, body)
	}

	t.Run("a plain member cannot issue an invitation", func(t *testing.T) {
		status, body := i.call(t, http.MethodPost, "/api/teams/t1/invitations", memberJWT,
			`{"email":"outsider@x","role":"member"}`)
		if status != http.StatusForbidden {
			t.Fatalf("member invite = %d body=%s, want 403", status, body)
		}
		// And nothing was created behind the refusal.
		status, body = i.call(t, http.MethodGet, "/api/teams/t1/invitations", adminJWT, "")
		if status != http.StatusOK {
			t.Fatalf("list = %d body=%s", status, body)
		}
		var got struct {
			Invitations []identity.Invitation `json:"invitations"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if len(got.Invitations) != 0 {
			t.Fatalf("refused invite still landed: %+v", got.Invitations)
		}
	})

	status, body := i.call(t, http.MethodPost, "/api/teams/t1/invitations", adminJWT,
		`{"email":"outsider@x","role":"member"}`)
	if status != http.StatusOK {
		t.Fatalf("admin invite = %d body=%s", status, body)
	}
	var invited struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(body, &invited); err != nil {
		t.Fatalf("decode invite: %v (%s)", err, body)
	}
	if invited.Token == "" || invited.ID == "" {
		t.Fatalf("invite returned no token/id: %s", body)
	}

	t.Run("the token is looked up anonymously, before any login", func(t *testing.T) {
		status, body := i.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(invited.Token), "", "")
		if status != http.StatusOK {
			t.Fatalf("anonymous lookup = %d body=%s", status, body)
		}
		var view struct {
			Email    string `json:"email"`
			Role     string `json:"role"`
			TeamID   string `json:"team_id"`
			TeamName string `json:"team_name"`
		}
		if err := json.Unmarshal(body, &view); err != nil {
			t.Fatalf("decode lookup: %v (%s)", err, body)
		}
		if view.TeamID != "t1" || view.Role != string(identity.RoleMember) || view.Email != "outsider@x" {
			t.Fatalf("lookup = %+v, want the invitation as issued", view)
		}
		if view.TeamName == "" {
			t.Fatal("lookup did not resolve the team name the invitee is shown")
		}
	})

	t.Run("an unknown token is refused, not answered with a team", func(t *testing.T) {
		if status, body := i.call(t, http.MethodGet, "/api/auth/invitations/lookup?token=not-a-real-token", "", ""); status == http.StatusOK {
			t.Fatalf("bogus token resolved: %d body=%s", status, body)
		}
	})

	t.Run("accepting grants exactly the invited role", func(t *testing.T) {
		status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", outsiderJWT,
			`{"token":"`+invited.Token+`"}`)
		if status != http.StatusOK {
			t.Fatalf("accept = %d body=%s", status, body)
		}
		var mb identity.Membership
		if err := json.Unmarshal(body, &mb); err != nil {
			t.Fatalf("decode membership: %v (%s)", err, body)
		}
		if mb.TeamID != "t1" || mb.UserID != "outsider" || mb.Role != identity.RoleMember {
			t.Fatalf("membership = %+v, want outsider as member of t1", mb)
		}

		// The boundary, after: the same request that was 403 now serves the
		// team, and the admin's member list names the new member.
		freshJWT := i.jwt(t, "outsider", "t1", identity.RoleMember)
		status, body = i.call(t, http.MethodGet, "/api/teams/t1/members", freshJWT, "")
		if status != http.StatusOK {
			t.Fatalf("invitee read after accept = %d body=%s, want 200", status, body)
		}
		var members struct {
			Members []struct {
				UserID string `json:"user_id"`
				Role   string `json:"role"`
			} `json:"members"`
		}
		if err := json.Unmarshal(body, &members); err != nil {
			t.Fatalf("decode members: %v", err)
		}
		found := false
		for _, m := range members.Members {
			if m.UserID == "outsider" {
				found = true
				if m.Role != string(identity.RoleMember) {
					t.Fatalf("invitee landed with role %q, want member", m.Role)
				}
			}
		}
		if !found {
			t.Fatalf("accepted invitee absent from the team: %+v", members.Members)
		}
	})

	t.Run("the token is single-use", func(t *testing.T) {
		status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", outsiderJWT,
			`{"token":"`+invited.Token+`"}`)
		if status == http.StatusOK {
			t.Fatalf("accepted token replayed successfully: %d body=%s", status, body)
		}
	})

	t.Run("accepting requires a login", func(t *testing.T) {
		status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", "",
			`{"token":"`+invited.Token+`"}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("anonymous accept = %d body=%s, want 401", status, body)
		}
	})
}

// A revoked invitation must stop working: the admin deletes it before the
// invitee gets round to it, and the token no longer resolves.
func TestInvitations_DeletingAPendingInviteKillsItsToken(t *testing.T) {
	i := newInviteE2E(t)
	adminJWT := i.jwt(t, "admin", "t1", identity.RoleAdmin)
	outsiderJWT := i.jwt(t, "outsider", "", "")

	status, body := i.call(t, http.MethodPost, "/api/teams/t1/invitations", adminJWT,
		`{"email":"outsider@x","role":"viewer"}`)
	if status != http.StatusOK {
		t.Fatalf("invite = %d body=%s", status, body)
	}
	var invited struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &invited); err != nil {
		t.Fatalf("decode invite: %v", err)
	}

	if status, body := i.call(t, http.MethodDelete, "/api/teams/t1/invitations/"+invited.ID, adminJWT, ""); status != http.StatusNoContent {
		t.Fatalf("delete invite = %d body=%s, want 204", status, body)
	}
	if status, body := i.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(invited.Token), "", ""); status == http.StatusOK {
		t.Fatalf("deleted invitation still resolves: %d body=%s", status, body)
	}
	if status, body := i.call(t, http.MethodPost, "/api/auth/invitations/accept", outsiderJWT,
		`{"token":"`+invited.Token+`"}`); status == http.StatusOK {
		t.Fatalf("deleted invitation still accepted: %d body=%s", status, body)
	}
	// The membership it would have granted does not exist.
	if _, err := i.s.authStore().GetMembership(context.Background(), "outsider", "t1"); err == nil {
		t.Fatal("a deleted invitation granted a membership anyway")
	}
}
