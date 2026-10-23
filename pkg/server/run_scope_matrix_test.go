package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run surfaces ask one computation twice: runSocketStanding (the CDP
// bridge's and the shell's ticks) and runConn.mutationAllowed (the run
// console, per mutating envelope) walk the same steps — the credential's
// standing, the run's team, the caller's standing in it with the super-admin
// reach read from the account the standing read just returned. This matrix
// drives BOTH paths over the same store states and asserts they answer the
// SAME verdict per state, then pins the two differences that are by design:
//
//   - the console has an envelope, so a lost scope refuses one envelope and
//     the socket stays; the bridge and the shell have none, so the same
//     verdict closes them (the credential refusal closes BOTH — its close is
//     the point — and an unreadable read closes neither);
//   - the console's cross-store skip (a `?store=` connection's mutating
//     envelopes are refused downstream, so its scope is not re-derived here)
//     has no counterpart on the shell and the bridge: their upgrade parses
//     no `?store=`, and a shell or a bridge cannot even open on a run that
//     is not this server's — the rows above close them on the LOCAL run's
//     state alone.
//
// Drift this test catches: a step added to one path and not the other, a
// verdict re-graded (a refusal becoming an outage or the reverse), a cause
// reworded, and the by-design differences quietly erased in either
// direction.
func TestRunScope_TheSocketSurfacesAndTheConsoleAnswerTheSameVerdictPerState(t *testing.T) {
	srv, guard := newRunScopeServer(t)
	gate := &scopeMatrixStore{Store: srv.authStore()}
	svc, err := auth.NewService(auth.Config{
		Store:      gate,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     srv.signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service over the gated store: %v", err)
	}
	srv.authSvc = svc

	// socketVerdict asks the path the bridge and the shell run on their tick.
	socketVerdict := func(id auth.Identity) socketTick {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), shellStandingTimeout)
		defer cancel()
		return srv.runSocketStanding(ctx, "run-1", id)
	}
	// consoleVerdict asks the path the run console runs per mutating
	// envelope, on a connection no socket drives: the answer is the envelope
	// on sendCh ("" code when allowed) plus what the write pump would close
	// with.
	consoleVerdict := func(id auth.Identity) (allowed bool, code, message string, closes bool) {
		t.Helper()
		c := newRunConn(srv, nil, "run-1")
		c.identity = id
		c.tenantID = "tenant-A"
		c.userID = id.UserID
		if c.mutationAllowed(runWSEnvelope{Type: wsTypeCancel, AckID: "m1"}) {
			return true, "", "", false
		}
		for {
			select {
			case data := <-c.sendCh:
				if len(data) == 0 {
					continue // the write pump's close marker, not an envelope
				}
				var env runWSEnvelope
				if err := json.Unmarshal(data, &env); err != nil {
					t.Fatalf("decode the console's envelope %s: %v", data, err)
				}
				if env.Type != wsTypeError {
					t.Fatalf("the console answered %q, want an error envelope", env.Type)
				}
				var p wsErrorPayload
				if err := json.Unmarshal(env.Payload, &p); err != nil {
					t.Fatalf("decode the console's error payload: %v", err)
				}
				return false, p.Code, p.Message, c.closeFrame != nil
			case <-time.After(5 * time.Second):
				t.Fatal("the console refused an envelope onto no channel")
			}
		}
	}

	// Row: a live member stands, and the console allows.
	if v := socketVerdict(caller("u-member-a", "tenant-A")); v.outcome != socketStands {
		t.Fatalf("a live member's socket verdict = %v (%q), want socketStands", v.outcome, v.cause)
	}
	if allowed, code, msg, closes := consoleVerdict(caller("u-member-a", "tenant-A")); !allowed || code != "" || msg != "" || closes {
		t.Fatalf("a live member's console verdict = allowed=%t %q/%q closes=%t, want allowed", allowed, code, msg, closes)
	}

	// Row: the standing read fails. No verdict for either path — and the
	// server's own failure closes neither.
	gate.failUser.Store(true)
	if v := socketVerdict(caller("u-member-a", "tenant-A")); v.outcome != socketUnreadable {
		t.Fatalf("a standing read that failed = %v (%q), want socketUnreadable", v.outcome, v.cause)
	}
	if allowed, code, msg, closes := consoleVerdict(caller("u-member-a", "tenant-A")); allowed || closes ||
		code != "standing_unavailable" || msg != "the credential of this connection could not be checked — retry" {
		t.Fatalf("a standing read that failed = allowed=%t %q/%q closes=%t, want standing_unavailable and the socket kept", allowed, code, msg, closes)
	}
	gate.failUser.Store(false)

	// Row: the caller's standing-in-team read fails. Same shape: no verdict,
	// nothing closed.
	gate.failMembership.Store(true)
	if v := socketVerdict(caller("u-member-a", "tenant-A")); v.outcome != socketUnreadable {
		t.Fatalf("a scope read that failed = %v (%q), want socketUnreadable", v.outcome, v.cause)
	}
	if allowed, code, msg, closes := consoleVerdict(caller("u-member-a", "tenant-A")); allowed || closes ||
		code != "scope_unavailable" || msg != "your standing in the run's team could not be resolved — retry" {
		t.Fatalf("a scope read that failed = allowed=%t %q/%q closes=%t, want scope_unavailable and the socket kept", allowed, code, msg, closes)
	}
	gate.failMembership.Store(false)
	if v := socketVerdict(caller("u-member-a", "tenant-A")); v.outcome != socketStands {
		t.Fatalf("after the reads recovered the member's verdict = %v, want socketStands", v.outcome)
	}

	// Row: the member is removed. The socket surfaces close scope_lost with
	// the standing cause; the console refuses the envelope and STAYS —
	// its by-design difference.
	if err := srv.authStore().DeleteMembership(context.Background(), "u-member-a", "tenant-A"); err != nil {
		t.Fatalf("remove the membership: %v", err)
	}
	if v := socketVerdict(caller("u-member-a", "tenant-A")); v.outcome != socketScopeLost || v.cause != causeStandingGone {
		t.Fatalf("a removed member's socket verdict = %v (%q), want socketScopeLost with %q", v.outcome, v.cause, causeStandingGone)
	}
	if allowed, code, msg, closes := consoleVerdict(caller("u-member-a", "tenant-A")); allowed || closes ||
		code != "forbidden" || msg != "your standing in the run's team no longer allows this action" {
		t.Fatalf("a removed member's console verdict = allowed=%t %q/%q closes=%t, want forbidden and the socket kept", allowed, code, msg, closes)
	}

	// Row: the demoted super-admin (sre holds no membership anywhere). The
	// reach is read from the account as the standing read returned it, and
	// both paths grade the loss the same way.
	no := false
	if _, err := srv.authStore().PatchUser(context.Background(), "sre", identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatalf("demote the account: %v", err)
	}
	demoted := caller("sre", "tenant-B")
	demoted.IsSuperAdmin = true
	if v := socketVerdict(demoted); v.outcome != socketScopeLost || v.cause != causeStandingGone {
		t.Fatalf("a demoted super-admin's socket verdict = %v (%q), want socketScopeLost with %q", v.outcome, v.cause, causeStandingGone)
	}
	if allowed, code, msg, closes := consoleVerdict(demoted); allowed || closes ||
		code != "forbidden" || msg != "your standing in the run's team no longer allows this action" {
		t.Fatalf("a demoted super-admin's console verdict = allowed=%t %q/%q closes=%t, want forbidden and the socket kept", allowed, code, msg, closes)
	}

	// Row: the account is disabled. A refusal, not an outage — and the ONE
	// state where the console closes too: the close IS the point of a
	// refusal on both surfaces.
	if err := srv.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "u-member-a", TeamID: "tenant-A", Role: identity.RoleMember,
	}); err != nil {
		t.Fatalf("re-grant the membership: %v", err)
	}
	disableAccount(t, srv, "u-member-a")
	if v := socketVerdict(caller("u-member-a", "tenant-A")); v.outcome != socketCredentialRefused || v.cause != auth.ErrAccountDisabled.Error() {
		t.Fatalf("a disabled account's socket verdict = %v (%q), want socketCredentialRefused with %q", v.outcome, v.cause, auth.ErrAccountDisabled.Error())
	}
	if allowed, code, msg, closes := consoleVerdict(caller("u-member-a", "tenant-A")); allowed || !closes ||
		code != wsCloseCredentialRefused || msg != auth.ErrAccountDisabled.Error() {
		t.Fatalf("a disabled account's console verdict = allowed=%t %q/%q closes=%t, want %q with %q and the close queued", allowed, code, msg, closes, wsCloseCredentialRefused, auth.ErrAccountDisabled.Error())
	}

	// Row: the run is gone. The scope verdict names the gone run; the console
	// refuses the envelope and stays. A LIVE member asks (the console's
	// standing half runs before the scope's).
	if err := guard.DeleteRun(store.WithTenant(context.Background(), "tenant-A"), "run-1"); err != nil {
		t.Fatalf("delete the run: %v", err)
	}
	if v := socketVerdict(caller("u-both", "tenant-A")); v.outcome != socketScopeLost || v.cause != causeRunGone {
		t.Fatalf("a gone run's socket verdict = %v (%q), want socketScopeLost with %q", v.outcome, v.cause, causeRunGone)
	}
	if allowed, code, msg, closes := consoleVerdict(caller("u-both", "tenant-A")); allowed || closes ||
		code != "forbidden" || msg != causeRunGone {
		t.Fatalf("a gone run's console verdict = allowed=%t %q/%q closes=%t, want forbidden naming the gone run and the socket kept", allowed, code, msg, closes)
	}

	// Row: the console's cross-store skip — a `?store=` connection's scope is
	// not re-derived here, so the gone run's envelopes are still allowed
	// through (refused downstream). The shell and the bridge have no such
	// row: their upgrade parses no `?store=`, and the rows above already
	// closed them on the local run's state alone.
	cross := newRunConn(srv, nil, "run-1")
	cross.identity = caller("u-both", "tenant-A")
	cross.tenantID = "tenant-A"
	cross.userID = "u-both"
	cross.xStore = guard
	if !cross.mutationAllowed(runWSEnvelope{Type: wsTypeCancel, AckID: "m1"}) {
		t.Fatal("a cross-store console's mutating envelope was refused here, want it passed to its downstream refusal")
	}
}

// scopeMatrixStore fails the reads the two paths ask while its test arms
// them, and passes everything else through.
type scopeMatrixStore struct {
	identity.Store
	failUser       atomic.Bool
	failMembership atomic.Bool
}

func (s *scopeMatrixStore) GetUser(ctx context.Context, id string) (identity.User, error) {
	if s.failUser.Load() {
		return identity.User{}, errors.New("identity: the store did not answer")
	}
	return s.Store.GetUser(ctx, id)
}

func (s *scopeMatrixStore) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if s.failMembership.Load() {
		return identity.Membership{}, errors.New("identity: the store did not answer")
	}
	return s.Store.GetMembership(ctx, userID, teamID)
}
