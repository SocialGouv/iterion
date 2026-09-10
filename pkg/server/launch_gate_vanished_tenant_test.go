package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// degradedTeamRead is an identity store whose team read can be switched to
// fail with something that is NOT ErrNotFound — a store outage rather than
// an answer. Switchable so a test can seed through the real store first and
// degrade only the read under test.
type degradedTeamRead struct {
	identity.Store
	err error
	on  *atomic.Bool
}

func (d degradedTeamRead) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	if d.on.Load() {
		return identity.Team{}, d.err
	}
	return d.Store.GetTeam(ctx, id)
}

// newDegradableServer is newOrgTestServer with a capturable log sink and a
// team read that can be broken mid-test, so the two arms of a failed
// identity read can be told apart by what the server DOES and by what it
// SAYS.
func newDegradableServer(t *testing.T) (*Server, *atomic.Bool, *bytes.Buffer) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	degrade := &atomic.Bool{}
	st := degradedTeamRead{Store: identity.NewMemoryStore(), err: context.DeadlineExceeded, on: degrade}
	svc, err := auth.NewService(auth.Config{
		Store:      st,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	logs := &bytes.Buffer{}
	s := New(Config{}, iterlog.New(iterlog.LevelWarn, logs))
	s.authSvc = svc
	return s, degrade, logs
}

// A JWT naming a team that no longer exists is a DEFINITE answer, not a
// blip: failing open let a stale token keep launching work under a tenant
// nobody can suspend, bill or see. The teamless arm already refuses that
// situation when the claim is empty; a vanished team is the same situation
// arriving later.
func TestGateLaunch_VanishedTeamIsDeniedNotFailedOpen(t *testing.T) {
	s, _, _ := newDegradableServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "team-that-was-deleted"})
	adm, d := s.gateLaunch(ctx)
	if d == nil {
		t.Fatalf("a vanished tenant was admitted (admission=%+v) — the launch runs unmetered under a team nobody owns", adm)
	}
	if d.status != 403 || d.reason != denyNoWorkspace {
		t.Fatalf("denial = %+v, want 403 %s", d, denyNoWorkspace)
	}
}

// A degraded read keeps the documented fail-open — quotas are operator
// policy and a transient store failure must not wedge every launch — but it
// may no longer be SILENT: the launch that follows is unmetered and
// un-suspendable, which is invisible from the outside otherwise.
func TestGateLaunch_DegradedTeamReadFailsOpenLoudly(t *testing.T) {
	s, degrade, logs := newDegradableServer(t)
	degrade.Store(true)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1"})
	if _, d := s.gateLaunch(ctx); d != nil {
		t.Fatalf("degraded read denied the launch (%+v) — the documented policy for a transient failure is fail-open", d)
	}
	got := logs.String()
	if !strings.Contains(got, "t1") || !strings.Contains(strings.ToLower(got), "ungated") {
		t.Fatalf("a launch admitted past a failed identity read logged nothing usable; logs = %q", got)
	}
}

// The same class one layer down: a PAT identity is FIXED for the token's
// whole life, so an org scope dropped because the store blipped is a lie it
// then carries everywhere. The membership read four lines above already
// refuses on error; the team read must not disagree with it.
func TestPATIdentity_DegradedTeamReadRefusesInsteadOfDroppingOrgScope(t *testing.T) {
	s, degrade, _ := newDegradableServer(t)
	s.pats = pat.NewMemoryStore()
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "u1", Email: "u1@x", Status: identity.UserStatusActive, DefaultTeamID: "t1",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u1", TeamID: "t1", Role: identity.RoleMember,
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	authed := auth.WithIdentity(ctx, auth.Identity{UserID: "u1", Email: "u1@x", TeamID: "t1", Role: identity.RoleMember})
	_, plaintext := createPAT(t, s, authed, `{"name":"ci"}`)

	degrade.Store(true)
	id, err := s.identityFromPAT(ctx, plaintext)
	if err == nil {
		t.Fatalf("a degraded team read minted identity %+v with org %q — an org-less PAT silently fails every org-scoped lookup that has no team fallback", id, id.OrgID)
	}
}
