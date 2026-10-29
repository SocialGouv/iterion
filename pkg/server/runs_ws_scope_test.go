package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/gorilla/websocket"
)

// The passive stream's half of the standing rule (the ADR-148 review's
// F1): subscribe/subscribe_logs carry no envelope of their own, so a
// subscribed connection re-derives its standing on a tick. Closing the
// run-visibility axis the reader leaned on ends the stream within one
// tick — the revocation the acceptance criterion promises.
func TestRunWS_ThePassiveStreamDiesWithTheAxis(t *testing.T) {
	t.Setenv("ITERION_RUN_WS_STANDING_TICK", "50ms")
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	ctx := context.Background()
	org := identity.RunVisibilityOrg
	if _, err := srv.authStore().PatchTeam(ctx, "tenant-A", identity.TeamPatch{RunVisibility: &org}); err != nil {
		t.Fatalf("open the axis: %v", err)
	}
	if _, err := srv.authStore().CreateUser(ctx, identity.User{ID: "u-orgmember", Email: "orgmember@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := srv.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{UserID: "u-orgmember", OrgID: "org-1", Role: identity.OrgRoleMember}); err != nil {
		t.Fatal(err)
	}
	id := auth.Identity{UserID: "u-orgmember", Email: "orgmember@x", TeamID: "tenant-B",
		Credential: auth.CredentialAccessToken, SessionLineage: "lin-orgmember"}
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"}); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, conn, 10*time.Second, wsTypeSnapshot)

	// Closing the axis ends the stream within one standing tick. Revoking
	// the org-membership fires the same tick — the axis only ever leaned
	// on it.
	empty := identity.RunVisibilityTeam
	if _, err := srv.authStore().PatchTeam(ctx, "tenant-A", identity.TeamPatch{RunVisibility: &empty}); err != nil {
		t.Fatal(err)
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.ClosePolicyViolation {
		t.Fatalf("close code = %d (%s), want a policy violation naming the lost scope", ce.Code, ce.Text)
	}
}

// The axis leans on the org-membership row, and the standing tick reads
// it LIVE: removing THAT row — not the flag — closes the passive stream
// within one tick too (the adversarial review's probe P1). The audience
// is the roster, so this is the revocation path an operator actually
// pulls.
func TestRunWS_ThePassiveStreamDiesWithTheOrgMembership(t *testing.T) {
	t.Setenv("ITERION_RUN_WS_STANDING_TICK", "50ms")
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	ctx := context.Background()
	org := identity.RunVisibilityOrg
	if _, err := srv.authStore().PatchTeam(ctx, "tenant-A", identity.TeamPatch{RunVisibility: &org}); err != nil {
		t.Fatalf("open the axis: %v", err)
	}
	if _, err := srv.authStore().CreateUser(ctx, identity.User{ID: "u-orgmember", Email: "orgmember@x", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := srv.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{UserID: "u-orgmember", OrgID: "org-1", Role: identity.OrgRoleMember}); err != nil {
		t.Fatal(err)
	}
	id := auth.Identity{UserID: "u-orgmember", Email: "orgmember@x", TeamID: "tenant-B",
		Credential: auth.CredentialAccessToken, SessionLineage: "lin-orgmember"}
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"}); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, conn, 10*time.Second, wsTypeSnapshot)

	// Revoking the ORG-MEMBERSHIP — the audience grant the axis leaned
	// on — fires the same tick: the stream dies with the row it stood on.
	if err := srv.authStore().DeleteOrgMembership(ctx, "u-orgmember", "org-1"); err != nil {
		t.Fatal(err)
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.ClosePolicyViolation {
		t.Fatalf("close code = %d (%s), want a policy violation naming the lost scope", ce.Code, ce.Text)
	}
}

// The run socket's scope — the run's team, the caller's standing THERE, the
// super-admin reach — is re-derived on every envelope that changes the run,
// from the account as the store reads it now. What the upgrade snapshotted
// does not steer the run for the socket's lifetime.

// TestRunWS_ADemotedSuperAdminsSocketSteersNoOtherTeamsRun: a super-admin's
// ticket opened ANOTHER team's run socket; the account is demoted mid-socket;
// the next mutating envelope is refused forbidden and the run is untouched.
func TestRunWS_ADemotedSuperAdminsSocketSteersNoOtherTeamsRun(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	ctx := context.Background()
	if _, err := srv.authStore().CreateUser(ctx, identity.User{ID: "u-super", Email: "super@x", Status: identity.UserStatusActive, IsSuperAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.authStore().GetMembership(ctx, "u-super", "tenant-A"); err == nil {
		t.Fatal("fixture: u-super is a member of tenant-A")
	}
	id := auth.Identity{UserID: "u-super", Email: "super@x", TeamID: "tenant-B", IsSuperAdmin: true,
		Credential: auth.CredentialAccessToken, SessionLineage: "lin-super"}
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	no := false
	if _, err := srv.authStore().PatchUser(ctx, "u-super", identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}); err != nil {
		t.Fatal(err)
	}
	env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError)
	if env.Type != wsTypeError {
		t.Fatalf("the demoted super-admin's cancel answered %q, want an error", env.Type)
	}
	if code := errorCodeOf(t, env).Code; code != "forbidden" {
		t.Fatalf("error code = %q, want forbidden", code)
	}
	if status := runStatusOf(t, srv, "run-1"); status == store.RunStatusCancelled {
		t.Fatal("the demoted super-admin's socket cancelled another team's run")
	}
	// The socket's reads are left alone: a subscribe still answers with the
	// snapshot.
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if env := awaitEnvelope(t, conn, 10*time.Second, wsTypeSnapshot, wsTypeError); env.Type != wsTypeSnapshot {
		t.Fatalf("the refused-but-open socket lost its reads: %q", env.Type)
	}
}

// TestRunWS_AMemberRemovedFromTheTeamSteersNoRun: a member removed from the
// run's team after the upgrade is refused at the next mutating envelope; a
// member the store still holds (control) cancels as before.
func TestRunWS_AMemberRemovedFromTheTeamSteersNoRun(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	ctx := context.Background()
	id := caller("u-member-a", "tenant-A")
	id.SessionLineage = "lin-a"
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	if err := srv.authStore().DeleteMembership(ctx, "u-member-a", "tenant-A"); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}); err != nil {
		t.Fatal(err)
	}
	env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError)
	if env.Type != wsTypeError {
		t.Fatalf("the removed member's cancel answered %q, want an error", env.Type)
	}
	if code := errorCodeOf(t, env).Code; code != "forbidden" {
		t.Fatalf("error code = %q, want forbidden", code)
	}
	if status := runStatusOf(t, srv, "run-1"); status == store.RunStatusCancelled {
		t.Fatal("the removed member's socket cancelled the run")
	}
}

// TestRunWS_ALiveMemberStillCancels: the per-envelope re-derivation keeps the
// ordinary path green — a live member of the run's team cancels and the ack
// names the new status.
func TestRunWS_ALiveMemberStillCancels(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	id := caller("u-member-a", "tenant-A")
	id.SessionLineage = "lin-a"
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError); env.Type != wsTypeAck {
		t.Fatalf("control: a live member's cancel answered %q %s", env.Type, string(env.Payload))
	}
	if status := runStatusOf(t, srv, "run-1"); status != store.RunStatusCancelled {
		t.Fatalf("control: the run did not cancel (status %s)", status)
	}
}

// membershipFlakyStore fails GetMembership once armed — the scope read the
// per-envelope re-derivation needs.
type membershipFlakyStore struct {
	identity.Store
	fail atomic.Bool
}

func (s *membershipFlakyStore) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if s.fail.Load() {
		return identity.Membership{}, errors.New("identity: the store did not answer")
	}
	return s.Store.GetMembership(ctx, userID, teamID)
}

// TestRunWS_AScopeReadThatFailsRefusesTheEnvelopeAndKeepsTheSocket: a scope
// read that did not complete is no verdict on the caller — the envelope is
// refused scope_unavailable and the socket's reads go on.
func TestRunWS_AScopeReadThatFailsRefusesTheEnvelopeAndKeepsTheSocket(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	gate := &membershipFlakyStore{Store: srv.authStore()}
	svc, err := auth.NewService(auth.Config{
		Store: gate, Sessions: auth.NewMemorySessionStore(), Signer: srv.signer,
		SignupMode: auth.SignupOpen, RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.authSvc = svc
	id := caller("u-member-a", "tenant-A")
	id.SessionLineage = "lin-a"
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	gate.fail.Store(true)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}); err != nil {
		t.Fatal(err)
	}
	env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError)
	if env.Type != wsTypeError {
		t.Fatalf("the failing scope read answered %q, want an error", env.Type)
	}
	if code := errorCodeOf(t, env).Code; code != "scope_unavailable" {
		t.Fatalf("error code = %q, want scope_unavailable", code)
	}
	gate.fail.Store(false)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if env := awaitEnvelope(t, conn, 10*time.Second, wsTypeSnapshot, wsTypeError); env.Type != wsTypeSnapshot {
		t.Fatalf("the socket did not survive the failed scope read: %q", env.Type)
	}
}
