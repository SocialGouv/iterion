package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// sessionsOf lists every stored session of a user.
func sessionsOf(t *testing.T, mem *MemorySessionStore, userID string) []Session {
	t.Helper()
	mem.mu.Lock()
	defer mem.mu.Unlock()
	var out []Session
	for id := range mem.byUserID[userID] {
		out = append(out, mem.byID[id])
	}
	return out
}

// liveSessionsOf counts the sessions of a user nothing revoked.
func liveSessionsOf(t *testing.T, mem *MemorySessionStore, userID string) int {
	t.Helper()
	n := 0
	for _, s := range sessionsOf(t, mem, userID) {
		if s.RevokedAt == nil {
			n++
		}
	}
	return n
}

// rotateLosingTheSuccessor runs a refresh of f.token whose rotation lands
// and whose successor's write fails: the client keeps the retired token,
// and no successor row exists.
func rotateLosingTheSuccessor(t *testing.T, f *rotationFixture, userAgent, ip string) {
	t.Helper()
	f.sessions.failCreateArmed.Store(true)
	if _, err := f.svc.Refresh(context.Background(), f.token, userAgent, ip); err == nil {
		t.Fatal("the refresh whose successor write failed reported success")
	}
	if f.session(t, f.token).RevokedAt == nil {
		t.Fatal("the rotation did not land")
	}
}

// TestLogout_ARetiredTokenIsReuseAfterASignOut drives the sign-out of a token
// a rotation already retired — the browser whose refresh answer was lost
// signs out with the token it still holds, or a thief signs out with a token
// stolen long after its rotation — and then that token presented past its
// window. It must be reuse, exactly as it was before the sign-out: the
// sign-out may not re-open the window and hand out the live successor.
func TestLogout_ARetiredTokenIsReuseAfterASignOut(t *testing.T) {
	for _, tc := range []struct {
		name          string
		signOutAfter  time.Duration
		presentedLate time.Duration
	}{
		{"the client signs out inside the window", 25 * time.Second, 20 * time.Second},
		{"a token stolen hours after its rotation", 6 * time.Hour, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRotationFixture(t, errStoreUnreachable)
			successor, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
			if err != nil {
				t.Fatalf("the rotation: %v", err)
			}
			f.clock.Advance(tc.signOutAfter)
			if err := f.svc.Logout(ctx, f.token); err != nil {
				t.Fatalf("Logout with the retired token: %v", err)
			}
			f.clock.Advance(tc.presentedLate)
			res, err := f.svc.Refresh(ctx, f.token, "attacker", "elsewhere")
			if !errors.Is(err, ErrSessionRevoked) {
				t.Fatalf("the retired token presented %v after its rotation, after a sign-out: got %v (successor delivered: %t), want ErrSessionRevoked",
					tc.signOutAfter+tc.presentedLate, err, err == nil && res.RefreshToken == successor.RefreshToken)
			}
			if s := f.session(t, successor.RefreshToken); s.RevokedAt == nil {
				t.Fatal("the successor of the signed-out lineage is live after the retired token was presented")
			}
			if f.otherDeviceRevoked(t) {
				t.Fatal("the retired token presented after a sign-out revoked the account's other device")
			}
		})
	}
}

// TestLogout_RevokesTheSuccessorItsTokenWasRotatedInto pins the sign-out's
// own guard, alone: signing out with a token a rotation retired ends the
// session that rotation wrote. The successor itself, presented after it, is
// refused — however long the window, its own token no longer refreshes.
func TestLogout_RevokesTheSuccessorItsTokenWasRotatedInto(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	successor, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the rotation: %v", err)
	}
	if err := f.svc.Logout(ctx, f.token); err != nil {
		t.Fatalf("Logout with the retired token: %v", err)
	}
	if f.session(t, successor.RefreshToken).RevokedAt == nil {
		t.Fatal("signing out with the retired token left its successor live")
	}
	if _, err := f.svc.Refresh(ctx, successor.RefreshToken, "ua", "ip"); err == nil {
		t.Fatal("the successor of a signed-out token still refreshes")
	}
}

// TestLogout_TakesTheSuccessorNotYetWritten pins the record's half of the
// sign-out, alone: a rotation whose successor's write failed leaves nothing
// to revoke, only the record that would write it. Signing out must clear
// that record, so the token presented afterwards writes no successor; the
// account's other device keeps its session.
func TestLogout_TakesTheSuccessorNotYetWritten(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	rotateLosingTheSuccessor(t, f, "ua", "ip")
	if err := f.svc.Logout(ctx, f.token); err != nil {
		t.Fatalf("Logout with the retired token: %v", err)
	}
	creates := f.sessions.creates.Load()
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the signed-out token presented again: got %v, want ErrSessionRevoked", err)
	}
	if n := f.sessions.creates.Load() - creates; n != 0 {
		t.Fatalf("the signed-out token wrote %d session(s): its successor outlived the sign-out", n)
	}
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("%d live session(s) remain in the signed-out lineage after its token was presented", n)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the signed-out token presented again revoked the account's other device")
	}
}

// TestRevokeUserSessions_TakesTheSuccessorNotYetWritten drives "sign out
// everywhere" (a password change or reset, an admin disable, the reuse
// detection of a row written before lineages) landing after a rotation whose
// successor's write failed.
// The retired token presented after it — inside its window — must find
// nothing to write: every session of the account is gone, the one that was
// never written included.
func TestRevokeUserSessions_TakesTheSuccessorNotYetWritten(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	rotateLosingTheSuccessor(t, f, "ua", "ip")
	f.clock.Advance(5 * time.Second)
	if err := f.svc.RevokeUserSessions(ctx, f.userID); err != nil {
		t.Fatalf("RevokeUserSessions: %v", err)
	}
	f.clock.Advance(5 * time.Second)
	creates := f.sessions.creates.Load()
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the retired token presented after every session was revoked: got %v (a token delivered: %t), want ErrSessionRevoked", err, res.RefreshToken != "")
	}
	if n := f.sessions.creates.Load() - creates; n != 0 {
		t.Fatalf("the presentation wrote %d session(s) after every session of the account was revoked", n)
	}
	f.svc.WaitRevocationPasses()
	if n := liveSessionsOf(t, f.sessions.MemorySessionStore, f.userID); n != 0 {
		t.Fatalf("%d live session(s) remain after every session of the account was revoked", n)
	}
}

// TestWriteSuccessor_ARevocationInFlightTakesTheSuccessor drives the write
// itself racing a revocation: a sign-out or a revoke-all that lands between a
// refresh's rotation and its successor's write sees no successor to revoke.
// The successor written after it must not survive it, nor be delivered.
func TestWriteSuccessor_ARevocationInFlightTakesTheSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(f *rotationFixture) error
	}{
		{"a revoke-all", func(f *rotationFixture) error {
			return f.svc.RevokeUserSessions(context.Background(), f.userID)
		}},
		{"a sign-out with the token being refreshed", func(f *rotationFixture) error {
			return f.svc.Logout(context.Background(), f.token)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRotationFixture(t, errStoreUnreachable)
			f.sessions.runBeforeNextCreate(func() {
				if err := tc.revoke(f); err != nil {
					t.Errorf("the revocation: %v", err)
				}
			})
			res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
			if !errors.Is(err, ErrSessionRevoked) {
				t.Fatalf("the refresh a revocation overtook: got %v (a token delivered: %t), want ErrSessionRevoked", err, res.RefreshToken != "")
			}
			for _, s := range sessionsOf(t, f.sessions.MemorySessionStore, f.userID) {
				if s.RevokedAt == nil && s.TokenHash != HashRefreshToken(f.other) {
					t.Fatalf("session %s (rotated from %q) is live after the revocation", s.ID, s.RotatedFromID)
				}
			}
		})
	}
}

// TestRefresh_ANeverWrittenSuccessorIsWrittenPastTheWindow drives the store
// failure the window exists for, answered late: the successor's write failed
// and the client comes back after the window — a store outage longer than
// it, a browser whose next request came minutes later. The rotation handed
// out nothing, so its token is still the only credential of its lineage:
// presenting it writes the successor, and revokes nothing.
func TestRefresh_ANeverWrittenSuccessorIsWrittenPastTheWindow(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	rotateLosingTheSuccessor(t, f, "ua", "ip")
	f.clock.Advance(DefaultRefreshReuseGrace + 15*time.Second)
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the retired token presented past the window, its successor never written: %v", err)
	}
	if s := f.session(t, res.RefreshToken); s.RevokedAt != nil {
		t.Fatal("the presentation delivered a revoked successor")
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("completing a successor that was never written revoked the account's other device")
	}
	// It completes the rotation once: a second holder presenting the same
	// token after it is reuse, as with any written successor past its window.
	if _, err := f.svc.Refresh(ctx, f.token, "other-holder", "ip2"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the token presented again once its successor was written: got %v, want ErrSessionRevoked", err)
	}
	if s := f.session(t, res.RefreshToken); s.RevokedAt == nil {
		t.Fatal("a second presentation after the completion was not read as reuse: the completed successor is live")
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("a second presentation after the completion revoked the account's other device")
	}
}

// TestRefresh_ANeverWrittenSuccessorEndsWithItsToken bounds the completion:
// a retired token whose successor was never written is answered as that
// token would be, so past its own expiry it is refused — expired, not
// reuse — and writes nothing. The sweep that removes a successor after its
// expiry cannot make an expired chain read as never written either: its
// token expired before it.
func TestRefresh_ANeverWrittenSuccessorEndsWithItsToken(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	rotateLosingTheSuccessor(t, f, "ua", "ip")
	f.clock.Advance(f.svc.refreshTTL + time.Minute)
	creates := f.sessions.creates.Load()
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("the retired token presented past its own expiry: got %v, want ErrSessionExpired", err)
	}
	if n := f.sessions.creates.Load() - creates; n != 0 {
		t.Fatalf("the expired token wrote %d session(s)", n)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("an expired token was read as reuse")
	}
}

// TestRefresh_ACompletedSuccessorIsTheRotations pins what the late write
// records: the client and the instant of the rotation it completes, not
// those of whoever presented the token — the session row is the trail an
// incident reads, and its lifetime runs from the rotation.
func TestRefresh_ACompletedSuccessorIsTheRotations(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	rotateLosingTheSuccessor(t, f, "rotation-client", "198.51.100.1")
	rotatedAt := *f.session(t, f.token).RevokedAt
	f.clock.Advance(10 * time.Second)
	res, err := f.svc.Refresh(ctx, f.token, "second-holder", "203.0.113.9")
	if err != nil {
		t.Fatalf("the completion: %v", err)
	}
	s := f.session(t, res.RefreshToken)
	if s.UserAgent != "rotation-client" || s.IP != "198.51.100.1" {
		t.Fatalf("the completed successor records ua=%q ip=%q, want the rotation's client", s.UserAgent, s.IP)
	}
	if !s.IssuedAt.Equal(rotatedAt) || !s.ExpiresAt.Equal(rotatedAt.Add(f.svc.refreshTTL)) {
		t.Fatalf("the completed successor runs %v → %v, want the rotation's %v → %v", s.IssuedAt, s.ExpiresAt, rotatedAt, rotatedAt.Add(f.svc.refreshTTL))
	}
	if !res.RefreshExpires.Equal(s.ExpiresAt) {
		t.Fatalf("the answer names the expiry %v, the row %v", res.RefreshExpires, s.ExpiresAt)
	}
}

// TestRefresh_TheWindowIsMeasuredOnTheStoresClock drives two replicas whose
// clocks disagree with the store's: the window follows the clock that
// stamped the rotation, not the clock of the replica answering. A retry two
// seconds after the rotation is inside it on a replica running 45 s ahead; a
// presentation 70 s after it is outside it on a replica running 60 s behind.
func TestRefresh_TheWindowIsMeasuredOnTheStoresClock(t *testing.T) {
	replica := func(t *testing.T, f *rotationFixture, skew time.Duration) *Service {
		t.Helper()
		svc, err := NewService(Config{Store: f.ids, Sessions: f.sessions, Signer: f.svc.signer, SignupMode: SignupOpen, RefreshTTL: f.svc.refreshTTL})
		if err != nil {
			t.Fatalf("replica: %v", err)
		}
		svc.now = func() time.Time { return f.clock.Now().Add(skew) }
		return svc
	}
	t.Run("a replica ahead answers a prompt retry inside the window", func(t *testing.T) {
		ctx := context.Background()
		f := newRotationFixture(t, errStoreUnreachable)
		first, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
		if err != nil {
			t.Fatalf("the rotation: %v", err)
		}
		f.clock.Advance(2 * time.Second)
		again, err := replica(t, f, 45*time.Second).Refresh(ctx, f.token, "ua", "ip")
		if err != nil || again.RefreshToken != first.RefreshToken {
			t.Fatalf("a retry 2 s after the rotation, on a replica 45 s ahead: err=%v, same successor=%t", err, again.RefreshToken == first.RefreshToken)
		}
		if f.otherDeviceRevoked(t) {
			t.Fatal("a prompt retry on a replica ahead revoked every session of the account")
		}
	})
	t.Run("a replica behind answers a late presentation as reuse", func(t *testing.T) {
		ctx := context.Background()
		f := newRotationFixture(t, errStoreUnreachable)
		if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err != nil {
			t.Fatalf("the rotation: %v", err)
		}
		f.clock.Advance(70 * time.Second)
		if _, err := replica(t, f, -60*time.Second).Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("a presentation 70 s after the rotation, on a replica 60 s behind: got %v, want ErrSessionRevoked", err)
		}
	})
}

// TestRefresh_AZeroGraceKeepsTheOldReuseDetection pins the override: with
// the window disabled, every presentation of a retired token is reuse —
// the successor that was never written included: nothing is written, the
// lineage is revoked, the other device is not.
func TestRefresh_AZeroGraceKeepsTheOldReuseDetection(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.svc.refreshReuseGrace = 0
	rotateLosingTheSuccessor(t, f, "ua", "ip")
	creates := f.sessions.creates.Load()
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a retired token with the window disabled: got %v, want ErrSessionRevoked", err)
	}
	if n := f.sessions.creates.Load() - creates; n != 0 {
		t.Fatalf("with the window disabled, a retired token wrote %d session(s)", n)
	}
	revoked, err := f.sessions.MemorySessionStore.SessionRevoked(ctx, f.session(t, f.token))
	if err != nil || !revoked {
		t.Fatalf("with the window disabled, a retired token was not reuse: its lineage is not revoked (err %v)", err)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("with the window disabled, a retired token revoked the account's other device")
	}
}

// TestNewService_RefusesAReuseGraceNotBelowTheRefreshTTL: a window as long as
// a session's lifetime would let the sweep of an expired successor read as
// a successor never written. The service refuses it, as it refuses a
// negative one.
func TestNewService_RefusesAReuseGraceNotBelowTheRefreshTTL(t *testing.T) {
	signer := newTestSigner(t)
	build := func(grace, ttl time.Duration) error {
		_, err := NewService(Config{Store: newNthCallGate(nil, nil), Sessions: NewMemorySessionStore(), Signer: signer, RefreshTTL: ttl, RefreshReuseGrace: &grace})
		return err
	}
	for _, grace := range []time.Duration{time.Hour, 2 * time.Hour} {
		if err := build(grace, time.Hour); err == nil || !strings.Contains(err.Error(), "refresh reuse grace") {
			t.Fatalf("a %v grace on a 1h refresh TTL: got %v, want a refusal", grace, err)
		}
	}
	if err := build(time.Hour-time.Second, time.Hour); err != nil {
		t.Fatalf("a grace under the refresh TTL was refused: %v", err)
	}
}

// TestNewService_WarnsAboutALongReuseGrace: a window far above the retries
// it exists for is accepted — the operator may mean it — and said at
// startup, since it is more often a unit typed wrong.
func TestNewService_WarnsAboutALongReuseGrace(t *testing.T) {
	for _, tc := range []struct {
		grace time.Duration
		warn  bool
	}{
		{30 * time.Second, false},
		{5 * time.Minute, false},
		{6 * time.Minute, true},
	} {
		var buf bytes.Buffer
		grace := tc.grace
		_, err := NewService(Config{
			Store: newNthCallGate(nil, nil), Sessions: NewMemorySessionStore(), Signer: newTestSigner(t),
			RefreshReuseGrace: &grace, Logger: iterlog.New(iterlog.LevelWarn, &buf),
		})
		if err != nil {
			t.Fatalf("a %v grace was refused: %v", tc.grace, err)
		}
		if got := strings.Contains(buf.String(), "refresh reuse grace"); got != tc.warn {
			t.Fatalf("a %v grace: warned=%t, want %t (log %q)", tc.grace, got, tc.warn, buf.String())
		}
	}
}
