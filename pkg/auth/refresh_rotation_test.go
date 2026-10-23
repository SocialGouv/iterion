package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestRefresh_ConcurrentPresentationsOfOneTokenShareItsSuccessor drives the
// case two tabs and a desktop produce every day: the same refresh token
// presented by several refreshes in flight at once. Each refresh that loses
// the race must receive the successor the winner minted — one session, one
// token — and never read as reuse, which revokes the lineage every caller
// holds.
func TestRefresh_ConcurrentPresentationsOfOneTokenShareItsSuccessor(t *testing.T) {
	for trial := 0; trial < 25; trial++ {
		f := newRotationFixture(t, errStoreUnreachable)
		f.sessions.latency.Store(int64(time.Millisecond))
		const callers = 4
		var (
			wg   sync.WaitGroup
			res  [callers]LoginResult
			errs [callers]error
			gate = make(chan struct{})
		)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-gate
				// Staggered starts cover both arms: a refresh reading the
				// session before the winner rotated it (it loses the
				// rotation), and one reading it after (it finds it rotated).
				time.Sleep(time.Duration(i) * 700 * time.Microsecond)
				res[i], errs[i] = f.svc.Refresh(context.Background(), f.token, "ua", "ip")
			}(i)
		}
		close(gate)
		wg.Wait()
		f.sessions.latency.Store(0)

		for i := 0; i < callers; i++ {
			if errs[i] != nil {
				t.Fatalf("trial %d: refresh %d of the same token failed: %v", trial, i, errs[i])
			}
			if res[i].RefreshToken != res[0].RefreshToken {
				t.Fatalf("trial %d: refresh %d got a different successor than refresh 0: one token forked into two sessions", trial, i)
			}
		}
		if f.otherDeviceRevoked(t) {
			t.Fatalf("trial %d: concurrent refreshes of one token revoked the account's other device", trial)
		}
		if s := f.session(t, res[0].RefreshToken); s.RevokedAt != nil {
			t.Fatalf("trial %d: the successor every caller holds is revoked", trial)
		}
	}
}

// TestRefresh_ReplayInsideTheWindowGetsTheSameSuccessor pins idempotence: a
// rotated token presented again inside the reuse grace window receives the
// very successor the rotation minted — not a new session, and not a revocation.
func TestRefresh_ReplayInsideTheWindowGetsTheSameSuccessor(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	first, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	createsAfterFirst := f.sessions.creates.Load()
	f.clock.Advance(DefaultRefreshReuseGrace - time.Second)
	again, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the rotated token presented inside the window: %v", err)
	}
	if again.RefreshToken != first.RefreshToken {
		t.Fatal("the replay inside the window got a successor other than the one the rotation minted")
	}
	if n := f.sessions.creates.Load(); n != createsAfterFirst {
		t.Fatalf("the replay wrote %d new session(s): the successor must be delivered again, not minted again", n-createsAfterFirst)
	}
	if again.AccessToken == "" || again.RefreshExpires.IsZero() {
		t.Fatalf("the replay delivered an incomplete login: %+v", again)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("a replay inside the window revoked the account's other device")
	}
	if s := f.session(t, again.RefreshToken); s.RevokedAt != nil {
		t.Fatal("a replay inside the window revoked the successor it delivered")
	}
}

// TestRefresh_ReplayAfterTheWindowRevokesTheLineage pins the other side of
// the window: past it, a rotated token presented again is reuse, and its
// lineage is revoked — the successor the rotation handed out, whoever holds
// it — while the account's other device, another lineage, keeps its session.
func TestRefresh_ReplayAfterTheWindowRevokesTheLineage(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	first, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	f.clock.Advance(DefaultRefreshReuseGrace + time.Second)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a rotated token presented after the window: got %v, want ErrSessionRevoked", err)
	}
	if s := f.session(t, first.RefreshToken); s.RevokedAt == nil {
		t.Fatal("reuse after the window left the successor live")
	}
	if _, err := f.svc.Refresh(ctx, first.RefreshToken, "ua", "ip"); err == nil {
		t.Fatal("the successor of a reused token still refreshes")
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("reuse after the window revoked the account's other device: the revocation took the account, not the lineage")
	}
	if _, err := f.svc.Refresh(ctx, f.other, "other-device", "ip2"); err != nil {
		t.Fatalf("the other device no longer refreshes after a reuse of another lineage: %v", err)
	}
}

// TestRefresh_SuccessorLostAfterTheRotationIsRecoverable drives the hazard of
// a store blip landing after the rotation: the successor's write fails, the
// client is answered an error and keeps the token it presented — which the
// desktop presents again on its next attempt. That presentation must
// complete the rotation, not read as reuse.
func TestRefresh_SuccessorLostAfterTheRotationIsRecoverable(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.failCreateArmed.Store(true)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil {
		t.Fatal("the refresh whose successor write failed reported success")
	}
	f.clock.Advance(15 * time.Second) // the desktop's retry floor
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the retry with the token the failed refresh left the client: %v", err)
	}
	if s := f.session(t, res.RefreshToken); s.RevokedAt != nil {
		t.Fatal("the retry delivered a revoked session")
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the retry after a lost successor revoked every session of the account")
	}
}

// TestRefresh_LoginReadsRunBeforeTheRotation pins the order: every read the
// login answer needs runs before the presented token is rotated, so a read
// that fails leaves the token live — never revoked under an error answer.
func TestRefresh_LoginReadsRunBeforeTheRotation(t *testing.T) {
	for _, read := range []string{"ListMembershipsByUser", "GetTeam", "GetOrgMembership", "ListOrgMembershipsByUser"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			f := newRotationFixture(t, context.Canceled)
			f.ids.fail(read, f.ids.count(read)+1)
			if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, context.Canceled) {
				t.Fatalf("a refresh whose %s fails: got %v, want that read's error", read, err)
			}
			if s := f.session(t, f.token); s.RevokedAt != nil {
				t.Fatalf("the presented token was rotated by a refresh that answered an error (%s failed)", read)
			}
			f.clock.Advance(DefaultRefreshReuseGrace + time.Minute)
			if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err != nil {
				t.Fatalf("the token survives the failed refresh but no longer refreshes: %v", err)
			}
			if f.otherDeviceRevoked(t) {
				t.Fatal("a failed read revoked the account's other device")
			}
		})
	}
}

// TestRefresh_RevokedWithoutRotationIsReuse pins the window's scope: it covers
// a token a ROTATION retired, never one a sign-out revoked. A logged-out token
// presented a second later — the sign-out's pass done — is reuse: refused,
// its lineage revoked, nothing written — and the account's other device
// untouched.
func TestRefresh_RevokedWithoutRotationIsReuse(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	if err := f.svc.Logout(ctx, f.token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	f.svc.WaitRevocationPasses()
	f.clock.Advance(time.Second)
	creates := f.sessions.creates.Load()
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a signed-out token presented inside the window: got %v, want ErrSessionRevoked", err)
	}
	if n := f.sessions.creates.Load() - creates; n != 0 {
		t.Fatalf("a signed-out token presented again wrote %d session(s)", n)
	}
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("%d live session(s) in the signed-out lineage", n)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("a signed-out token presented again revoked the account's other device")
	}
}

// TestRefresh_SuccessorRotatedAgainIsReuse pins what the window re-delivers:
// only a successor still live and never rotated. Once the successor was
// itself rotated, presenting the first token again is reuse: the lineage —
// C, the session B's rotation wrote, included — is revoked, and the other
// device is not.
func TestRefresh_SuccessorRotatedAgainIsReuse(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	b, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("A→B: %v", err)
	}
	f.clock.Advance(time.Second)
	c, err := f.svc.Refresh(ctx, b.RefreshToken, "ua", "ip")
	if err != nil {
		t.Fatalf("B→C: %v", err)
	}
	f.clock.Advance(time.Second)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("A presented after its successor rotated: got %v, want ErrSessionRevoked", err)
	}
	if s := f.session(t, c.RefreshToken); s.RevokedAt == nil {
		t.Fatal("A presented after its successor rotated left C, the lineage's live session, live")
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("A presented after its successor rotated revoked the account's other device")
	}
}

// TestRefresh_TheWindowRechecksTheAccount pins that the window re-delivers a
// session, never an authorization: an account disabled since the rotation
// gets nothing.
func TestRefresh_TheWindowRechecksTheAccount(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	disabled := identity.UserStatusDisabled
	if _, err := f.svc.store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &disabled}); err != nil {
		t.Fatalf("disable the account: %v", err)
	}
	f.clock.Advance(time.Second)
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("a replay inside the window for a disabled account: got %v (token delivered: %t), want ErrAccountDisabled",
			err, res.RefreshToken != "")
	}
}

// TestRefresh_SuccessorNeedsTheServerKey pins what the stored rotation
// record is worth without the server's key: nothing. A service holding the
// same session rows under another signing key cannot re-derive the
// successor, and falls back to reuse detection rather than to a guess.
func TestRefresh_SuccessorNeedsTheServerKey(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	stranger := newTestServiceOn(t, f.ids, f.sessions)
	stranger.now = f.clock.Now
	f.clock.Advance(time.Second)
	if _, err := stranger.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a service under another key re-delivered the successor (err=%v): the derivation does not depend on the server key", err)
	}
}

// TestRefresh_NoRawTokenAtRest pins the storage rule the window must keep:
// what a rotation writes holds hashes and a nonce, never a refresh token a
// reader of the store could present.
func TestRefresh_NoRawTokenAtRest(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	f.sessions.mu.Lock()
	defer f.sessions.mu.Unlock()
	for _, s := range f.sessions.byID {
		for _, raw := range []string{f.token, res.RefreshToken} {
			for name, v := range map[string]string{
				"id": s.ID, "token_hash": s.TokenHash, "rotation_nonce": s.RotationNonce,
				"rotation_kid": s.RotationKeyID, "rotated_from": s.RotatedFromID, "successor_hash": s.SuccessorHash,
			} {
				if v != "" && strings.Contains(v, raw) {
					t.Fatalf("session %s stores a raw refresh token in %s", s.ID, name)
				}
			}
		}
	}
}

// TestRefresh_UnknownOrOrphanedSessionIsNotFound pins the two refusals a
// client must read as "this credential is gone": a token the store has no
// row for (the TTL index deleted an expired session), and a session whose
// account no longer exists.
func TestRefresh_UnknownOrOrphanedSessionIsNotFound(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	if _, err := f.svc.Refresh(ctx, "a-token-no-session-was-ever-issued-for", "ua", "ip"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown token: got %v, want ErrSessionNotFound", err)
	}
	orphan, _, err := IssueSession(ctx, f.sessions, NewAccountRoot(), "no-such-user", "ua", "ip", time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, orphan, "ua", "ip"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("a session whose account is gone: got %v, want ErrSessionNotFound", err)
	}
}

// TestRefresh_SuccessorOfAnotherLineageIsRefused: the row the window finds
// under the derived successor's hash must be the successor of the session
// presented. A row of another lineage there is refused as the integrity
// failure it is — never delivered.
func TestRefresh_SuccessorOfAnotherLineageIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	b, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	f.sessions.mu.Lock()
	id := f.sessions.byHash[HashRefreshToken(b.RefreshToken)]
	row := f.sessions.byID[id]
	row.RotatedFromID = "another-session"
	f.sessions.byID[id] = row
	f.sessions.mu.Unlock()
	f.clock.Advance(time.Second)
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err == nil || errors.Is(err, ErrSessionRevoked) || res.RefreshToken != "" {
		t.Fatalf("a successor row of another lineage: got err=%v token delivered=%t, want an integrity error", err, res.RefreshToken != "")
	}
}

// TestRefresh_SuccessorWrittenByAConcurrentRefreshIsDelivered: two refreshes
// of one token derive the same successor, and the one whose write comes
// second meets the first one's row. That row — the same lineage — is the
// one delivered.
func TestRefresh_SuccessorWrittenByAConcurrentRefreshIsDelivered(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	var written Session
	f.sessions.preemptNextCreate(func(next Session) Session {
		written = next
		written.ID = "written-by-the-other-refresh"
		written.ExpiresAt = next.ExpiresAt.Add(-time.Minute)
		return written
	})
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("a refresh whose successor another refresh wrote first: %v", err)
	}
	if HashRefreshToken(res.RefreshToken) != written.TokenHash || !res.RefreshExpires.Equal(written.ExpiresAt) {
		t.Fatal("the refresh delivered a session other than the row written first")
	}
}

// TestRefresh_ACollidingSuccessorOfAnotherLineageIsRefused: the row met
// under the successor's hash at write time must be that successor. One of
// another lineage is refused, never delivered.
func TestRefresh_ACollidingSuccessorOfAnotherLineageIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.preemptNextCreate(func(next Session) Session {
		intruder := next
		intruder.ID = "intruder"
		intruder.RotatedFromID = "another-session"
		return intruder
	})
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err == nil || res.RefreshToken != "" {
		t.Fatalf("a colliding row of another lineage: got err=%v token delivered=%t, want an integrity error", err, res.RefreshToken != "")
	}
}

// TestRefresh_SuccessorExpiredIsReuse: the window re-delivers a live
// successor only. One past its own expiry is not re-delivered: the
// presentation is reuse. The store's clock — the window's — stands still
// while the service's moves past the successor's expiry, so the presentation
// is inside the window and only the successor's expiry refuses it.
func TestRefresh_SuccessorExpiredIsReuse(t *testing.T) {
	ctx := context.Background()
	clock := &testClock{now: time.Now().UTC()}
	storeClock := &testClock{now: clock.Now()}
	sessions := newGatedSessions()
	sessions.SetClock(storeClock.Now)
	grace := 500 * time.Millisecond
	svc, err := NewService(Config{
		Store: identity.NewMemoryStore(), Sessions: sessions, Signer: newTestSigner(t),
		SignupMode: SignupOpen, RefreshTTL: time.Second, RefreshReuseGrace: &grace,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = clock.Now
	reg, err := svc.Register(ctx, "short@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.Refresh(ctx, reg.RefreshToken, "ua", "ip"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := svc.Refresh(ctx, reg.RefreshToken, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a retired token whose successor expired, presented inside the window: got %v, want ErrSessionRevoked", err)
	}
}

// TestRefresh_TheWindowsPrice pins the tradeoff the window is documented
// with: a stolen retired token replayed inside the window receives the same
// successor the legitimate holder received — and from then on, a retired
// token presented outside its window by either holder revokes the lineage
// both hold, the legitimate one's session included; the account's other
// device keeps its own.
func TestRefresh_TheWindowsPrice(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	legit, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the legitimate refresh: %v", err)
	}
	f.clock.Advance(5 * time.Second)
	stolen, err := f.svc.Refresh(ctx, f.token, "attacker", "elsewhere")
	if err != nil || stolen.RefreshToken != legit.RefreshToken {
		t.Fatalf("the replay inside the window: err=%v, same successor=%t", err, stolen.RefreshToken == legit.RefreshToken)
	}
	f.clock.Advance(15 * time.Minute)
	next, err := f.svc.Refresh(ctx, legit.RefreshToken, "ua", "ip")
	if err != nil {
		t.Fatalf("the legitimate holder's next rotation: %v", err)
	}
	f.clock.Advance(DefaultRefreshReuseGrace + time.Second)
	if _, err := f.svc.Refresh(ctx, stolen.RefreshToken, "attacker", "elsewhere"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the other holder's retired token outside its window: got %v, want ErrSessionRevoked", err)
	}
	if s := f.session(t, next.RefreshToken); s.RevokedAt == nil {
		t.Fatal("the legitimate holder's session survived the reuse")
	}
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("the reuse left %d live session(s) in the lineage both holders shared", n)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the reuse revoked the account's other device")
	}
}

// TestRefresh_ASignedOutTokenPresentedBeforeItsPassIsRefused: the sign-out's
// record is its commit point. The token presented after the record and
// before the pass reached it is still live in the store: its refresh rotates
// it, and the successor it writes is refused at its delivery and born
// revoked — no session of the lineage refreshes, and the other device keeps
// its session.
func TestRefresh_ASignedOutTokenPresentedBeforeItsPassIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	release := f.sessions.holdPasses()
	if err := f.svc.Logout(ctx, f.token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a signed-out token presented before its pass: got %v (a token delivered: %t), want ErrSessionRevoked", err, res.RefreshToken != "")
	}
	release()
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("%d live session(s) in the signed-out lineage", n)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the sign-out revoked the account's other device")
	}
}
