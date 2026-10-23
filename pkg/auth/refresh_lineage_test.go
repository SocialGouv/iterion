package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestLogout_EndsTheWholeLineage drives the sign-out of a token whose lineage
// grew past its own successor: T's holder lost the answer of T's rotation
// and still holds T, a second holder received S inside the window and
// rotated it into S2. Signing out with T must end S2 too — the lineage, not
// one hop of it — and leave the account's other device signed in.
func TestLogout_EndsTheWholeLineage(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	s, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("T→S: %v", err)
	}
	f.clock.Advance(time.Second)
	s2, err := f.svc.Refresh(ctx, s.RefreshToken, "second-holder", "ip2")
	if err != nil {
		t.Fatalf("S→S2: %v", err)
	}
	if err := f.svc.Logout(ctx, f.token); err != nil {
		t.Fatalf("Logout with T: %v", err)
	}
	if f.session(t, s2.RefreshToken).RevokedAt == nil {
		t.Fatal("the sign-out with T left S2, its lineage's live descendant, live")
	}
	if _, err := f.svc.Refresh(ctx, s2.RefreshToken, "second-holder", "ip2"); err == nil {
		t.Fatal("S2 still refreshes after the sign-out of its lineage")
	}
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("%d live session(s) in the signed-out lineage", n)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the sign-out of one lineage signed the account's other device out")
	}
}

// TestRefresh_TheCompletionHorizonBoundsANeverWrittenSuccessor drives a
// rotation whose successor's write failed: inside the completion horizon the
// retired token completes it — a client retrying late; past it the token
// was abandoned — its holder signed in again some other way — and whoever
// presents it now is reuse: nothing written, the lineage revoked, the other
// device untouched.
func TestRefresh_TheCompletionHorizonBoundsANeverWrittenSuccessor(t *testing.T) {
	t.Run("inside the horizon", func(t *testing.T) {
		ctx := context.Background()
		f := newRotationFixture(t, errStoreUnreachable)
		rotateLosingTheSuccessor(t, f, "ua", "ip")
		f.clock.Advance(completionHorizon - time.Second)
		res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
		if err != nil {
			t.Fatalf("the retired token presented inside the completion horizon: %v", err)
		}
		if f.session(t, res.RefreshToken).RevokedAt != nil {
			t.Fatal("the completion delivered a revoked successor")
		}
		if f.otherDeviceRevoked(t) {
			t.Fatal("the completion revoked the account's other device")
		}
	})
	t.Run("past the horizon", func(t *testing.T) {
		ctx := context.Background()
		f := newRotationFixture(t, errStoreUnreachable)
		rotateLosingTheSuccessor(t, f, "ua", "ip")
		f.clock.Advance(completionHorizon + time.Second)
		creates := f.sessions.creates.Load()
		res, err := f.svc.Refresh(ctx, f.token, "elsewhere", "203.0.113.9")
		if !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("a token abandoned past the completion horizon: got %v (a session delivered: %t), want ErrSessionRevoked", err, res.RefreshToken != "")
		}
		if n := f.sessions.creates.Load() - creates; n != 0 {
			t.Fatalf("a token abandoned past the completion horizon wrote %d session(s)", n)
		}
		if n := f.liveInLineage(t, f.token); n != 0 {
			t.Fatalf("%d live session(s) in the abandoned token's lineage", n)
		}
		if f.otherDeviceRevoked(t) {
			t.Fatal("the reuse of an abandoned token revoked the account's other device")
		}
	})
	t.Run("a window longer than the horizon extends it", func(t *testing.T) {
		ctx := context.Background()
		f := newRotationFixture(t, errStoreUnreachable)
		f.svc.refreshReuseGrace = completionHorizon + time.Minute
		rotateLosingTheSuccessor(t, f, "ua", "ip")
		f.clock.Advance(completionHorizon + 30*time.Second)
		if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err != nil {
			t.Fatalf("a never-written successor inside a window longer than the horizon: %v", err)
		}
	})
}

// TestRefresh_AFailedCheckLeavesTheSuccessorHandedToNobody drives a store
// that fails the successor's check after its write landed: the client is
// answered an error and keeps its token, and the store answers again only
// after the window has closed. The successor stays written and unclaimed —
// handed to nobody — so the late presentation, inside the completion
// horizon, completes it instead of reading as reuse: both the client's
// lineage and the other device keep their sessions.
func TestRefresh_AFailedCheckLeavesTheSuccessorHandedToNobody(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.failCheckArmed.Store(true)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil || errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the refresh whose successor's check failed: got %v, want the store's failure", err)
	}
	written := successorsOf(t, f, f.token)
	if len(written) != 1 || written[0].DeliveredAt != nil {
		t.Fatalf("after the failed check the token's successors read %+v, want one, never claimed", written)
	}
	f.clock.Advance(DefaultRefreshReuseGrace + 30*time.Second)
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the re-presentation once the store answers, past the window: %v", err)
	}
	got := f.session(t, res.RefreshToken)
	if got.RevokedAt != nil || got.DeliveredAt == nil || got.ID != written[0].ID {
		t.Fatalf("the re-presentation delivered %+v, want the written successor %s, live and claimed", got, written[0].ID)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the failed check revoked the account's other device")
	}
}

// TestRefresh_AWriteWhoseAnswerWasLostLeavesTheSuccessorHandedToNobody: the
// successor's write landed but its answer did not reach the service. The
// row stays, unclaimed, and the late presentation — past the window, inside
// the completion horizon — completes the rotation with it.
func TestRefresh_AWriteWhoseAnswerWasLostLeavesTheSuccessorHandedToNobody(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.landThenFailCreateArmed.Store(true)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil {
		t.Fatal("the refresh whose write answered an error reported success")
	}
	written := successorsOf(t, f, f.token)
	if len(written) != 1 || written[0].DeliveredAt != nil {
		t.Fatalf("after the lost answer the token's successors read %+v, want one, never claimed", written)
	}
	f.clock.Advance(DefaultRefreshReuseGrace + 30*time.Second)
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the re-presentation past the window: %v", err)
	}
	if got := f.session(t, res.RefreshToken); got.RevokedAt != nil || got.ID != written[0].ID {
		t.Fatalf("the re-presentation delivered %+v, want the written successor %s, live", got, written[0].ID)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the lost write revoked the account's other device")
	}
}

// TestRefresh_EverySessionStepIsBounded pins the bounds of a refresh — a
// rotation, a redelivery, a completion, a reuse, a disabled account's
// refusal: every session-store step carries a deadline no further than its
// own bound (a revocation's, or sessionOpTimeout), and every identity read
// one no further than the refresh's (RefreshTimeout), so a store that stalls
// answers a failure within seconds instead of after the window.
func TestRefresh_EverySessionStepIsBounded(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	ids := &deadlineIdentity{Store: f.ids}
	f.svc.SetStoreForTest(ids)
	f.sessions.resetDeadlines()
	first, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the rotation: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err != nil {
		t.Fatalf("the redelivery: %v", err)
	}
	f.sessions.failCreateArmed.Store(true)
	if _, err := f.svc.Refresh(ctx, first.RefreshToken, "ua", "ip"); err == nil {
		t.Fatal("the rotation whose write failed reported success")
	}
	if _, err := f.svc.Refresh(ctx, first.RefreshToken, "ua", "ip"); err != nil {
		t.Fatalf("the completion: %v", err)
	}
	f.clock.Advance(completionHorizon + time.Minute)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the reuse: got %v, want ErrSessionRevoked", err)
	}
	setStatus(t, f, identity.UserStatusDisabled)
	if _, err := f.svc.Refresh(ctx, f.other, "other-device", "ip2"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("the disabled account's refresh: got %v, want ErrAccountDisabled", err)
	}
	f.svc.WaitRevocationPasses()
	bound := map[string]time.Duration{"RevokeLineageSessions": revocationTimeout, "RevokeAccountSessions": revocationTimeout}
	seen := f.sessions.recordedDeadlines()
	for _, m := range []string{"GetSessionByTokenHash", "RotateSession", "CreateSession", "SessionRevoked", "ClaimSession", "Now", "RecordLineageRevocation", "RevokeLineageSessions", "RevokeSession"} {
		if len(seen[m]) == 0 {
			t.Fatalf("the refreshes never called %s: the witness does not drive the step it pins", m)
		}
	}
	for m, aheads := range seen {
		limit := sessionOpTimeout
		if b, ok := bound[m]; ok {
			limit = b
		}
		for i, ahead := range aheads {
			if ahead <= 0 || ahead > limit {
				t.Fatalf("call %d of %s ran %v ahead of its deadline, want within (0, %v]", i, m, ahead, limit)
			}
		}
	}
	reads := ids.recorded()
	if len(reads) == 0 {
		t.Fatal("the refreshes read no identity row: the witness does not drive the reads it pins")
	}
	for i, ahead := range reads {
		if ahead <= 0 || ahead > RefreshTimeout {
			t.Fatalf("identity read %d of the refreshes ran %v ahead of its deadline, want within (0, %v]", i, ahead, RefreshTimeout)
		}
	}
}

// deadlineIdentity is an identity store that records, for every read a
// refresh makes, how far ahead of the call its context's deadline was (zero
// without one).
type deadlineIdentity struct {
	identity.Store
	mu     sync.Mutex
	aheads []time.Duration
}

func (d *deadlineIdentity) note(ctx context.Context) {
	var ahead time.Duration
	if dl, ok := ctx.Deadline(); ok {
		ahead = time.Until(dl)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.aheads = append(d.aheads, ahead)
}

func (d *deadlineIdentity) recorded() []time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Duration(nil), d.aheads...)
}

func (d *deadlineIdentity) GetUser(ctx context.Context, id string) (identity.User, error) {
	d.note(ctx)
	return d.Store.GetUser(ctx, id)
}

func (d *deadlineIdentity) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	d.note(ctx)
	return d.Store.ListMembershipsByUser(ctx, userID)
}

func (d *deadlineIdentity) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	d.note(ctx)
	return d.Store.GetTeam(ctx, id)
}

func (d *deadlineIdentity) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	d.note(ctx)
	return d.Store.GetOrgMembership(ctx, userID, orgID)
}

func (d *deadlineIdentity) ListOrgMembershipsByUser(ctx context.Context, userID string) ([]identity.OrgMembership, error) {
	d.note(ctx)
	return d.Store.ListOrgMembershipsByUser(ctx, userID)
}

// TestRefresh_ARevokedSuccessorAnotherRefreshWroteIsNotDelivered: the row a
// concurrent refresh of the same token wrote first is the one delivered —
// unless it no longer is a session: revoked since, it is refused, never
// handed out.
func TestRefresh_ARevokedSuccessorAnotherRefreshWroteIsNotDelivered(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.preemptNextCreate(func(next Session) Session {
		written := next
		written.ID = "written-by-the-other-refresh"
		at := f.clock.Now()
		written.RevokedAt = &at
		return written
	})
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if !errors.Is(err, ErrSessionRevoked) || res.RefreshToken != "" {
		t.Fatalf("a revoked successor another refresh wrote: got err=%v, token delivered=%t, want ErrSessionRevoked and nothing", err, res.RefreshToken != "")
	}
}

// TestRefresh_ARedeliveryChecksTheRevocations drives a revocation of the
// lineage landing between a redelivery's read of the successor and its
// answer: the redelivery must not hand out a successor its lineage's
// revocation covers.
func TestRefresh_ARedeliveryChecksTheRevocations(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	first, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if err != nil {
		t.Fatalf("the rotation: %v", err)
	}
	f.clock.Advance(time.Second)
	f.sessions.runBeforeNextCheck(func() {
		if err := f.svc.Logout(context.Background(), first.RefreshToken); err != nil {
			t.Errorf("the sign-out: %v", err)
		}
	})
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a redelivery a sign-out of its lineage overtook: got %v (a session delivered: %t), want ErrSessionRevoked", err, res.RefreshToken != "")
	}
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("%d live session(s) in the signed-out lineage", n)
	}
}

// TestIssueSession_ALoginRacingARevokeAllIsCovered drives a login whose root
// is written after a revoke-all's pass ran — the account's password changed
// while the login was in flight: the root was born under the generation
// the revoke-all raised, so it renews nothing.
func TestIssueSession_ALoginRacingARevokeAllIsCovered(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.runBeforeNextCreate(func() {
		if err := f.svc.RevokeUserSessions(context.Background(), f.userID); err != nil {
			t.Errorf("the revoke-all: %v", err)
		}
	})
	raced, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "racing-login", "ip3")
	if err != nil {
		t.Fatalf("the login: %v", err)
	}
	res, err := f.svc.Refresh(ctx, raced.RefreshToken, "racing-login", "ip3")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the first refresh of a login a revoke-all overtook: got %v (a session delivered: %t), want ErrSessionRevoked", err, res.RefreshToken != "")
	}
	f.svc.WaitRevocationPasses()
	if n := liveSessionsOf(t, f.sessions.MemorySessionStore, f.userID); n != 0 {
		t.Fatalf("%d live session(s) after the revoke-all and the raced login's refresh", n)
	}
}

// TestIssueSession_ALoginAfterARevokeAllRefreshes is the legitimate side of
// the generation a revoke-all raises: a login made after it — the password
// change's own new session, a sign-in on another device — is born under the
// raised generation, and its rotations are delivered.
func TestIssueSession_ALoginAfterARevokeAllRefreshes(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	if err := f.svc.RevokeUserSessions(ctx, f.userID); err != nil {
		t.Fatalf("the revoke-all: %v", err)
	}
	after, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "after", "ip4")
	if err != nil {
		t.Fatalf("the login after the revoke-all: %v", err)
	}
	next, err := f.svc.Refresh(ctx, after.RefreshToken, "after", "ip4")
	if err != nil {
		t.Fatalf("the first refresh of a login made after a revoke-all: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, next.RefreshToken, "after", "ip4"); err != nil {
		t.Fatalf("the second refresh of a login made after a revoke-all: %v", err)
	}
}

// TestRefresh_ARowWrittenBeforeLineagesIsItsOwnRoot drives the rows a
// deployment without lineages left behind. A live one rotates into a
// successor of its own lineage, and its reuse then revokes that lineage
// only. A retired one — a rotation of that version recorded no link to its
// successor — names no family: its reuse revokes the account, as reuse
// detection did then.
func TestRefresh_ARowWrittenBeforeLineagesIsItsOwnRoot(t *testing.T) {
	legacy := func(t *testing.T, f *rotationFixture, retired bool) string {
		t.Helper()
		raw, _, err := GenerateRandomToken(refreshTokenBytes)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		now := f.clock.Now()
		row := Session{ID: "legacy-" + raw[:8], UserID: f.userID, TokenHash: HashRefreshToken(raw), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
		if retired {
			at := now.Add(-time.Hour)
			row.RevokedAt = &at
		}
		if err := f.sessions.MemorySessionStore.CreateSession(context.Background(), row); err != nil {
			t.Fatalf("seed the legacy row: %v", err)
		}
		return raw
	}
	t.Run("a live one rotates into its own lineage", func(t *testing.T) {
		ctx := context.Background()
		f := newRotationFixture(t, errStoreUnreachable)
		raw := legacy(t, f, false)
		next, err := f.svc.Refresh(ctx, raw, "ua", "ip")
		if err != nil {
			t.Fatalf("the rotation of a legacy row: %v", err)
		}
		root := f.session(t, raw)
		if root.LineageID != root.ID || f.session(t, next.RefreshToken).LineageID != root.ID {
			t.Fatalf("the legacy root reads lineage %q and its successor %q, want both %q", root.LineageID, f.session(t, next.RefreshToken).LineageID, root.ID)
		}
		f.clock.Advance(DefaultRefreshReuseGrace + time.Second)
		if _, err := f.svc.Refresh(ctx, raw, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("the legacy row presented past its window: got %v, want ErrSessionRevoked", err)
		}
		if f.session(t, next.RefreshToken).RevokedAt == nil {
			t.Fatal("the reuse of a legacy row left its successor live")
		}
		if f.otherDeviceRevoked(t) {
			t.Fatal("the reuse of a legacy row the new code rotated revoked the account's other device")
		}
	})
	t.Run("a retired one names no family", func(t *testing.T) {
		ctx := context.Background()
		f := newRotationFixture(t, errStoreUnreachable)
		raw := legacy(t, f, true)
		if _, err := f.svc.Refresh(ctx, raw, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("a retired legacy row: got %v, want ErrSessionRevoked", err)
		}
		if !f.otherDeviceRevoked(t) {
			t.Fatal("the reuse of a retired legacy row left the account's other sessions live: its family cannot be named, the account must be revoked")
		}
	})
}

// TestRefresh_ALegitimateStragglerCostsAtMostItsLineage drives the four ways
// a legitimate client presents a retired token late: a lost answer re-
// presented minutes later, a tab presenting after another tab's needless
// rotation, a successor's check that failed after its write, and a store
// slow enough for the client to give up after the rotation. Each costs at
// most the client's own lineage — the account's other device keeps its
// session — and the two whose successor was never confirmed cost nothing.
func TestRefresh_ALegitimateStragglerCostsAtMostItsLineage(t *testing.T) {
	for _, tc := range []struct {
		name string
		// straggle runs the sequence and presents the straggler, answering
		// whether the client's own lineage survived.
		straggle func(t *testing.T, f *rotationFixture) (lineageLives bool)
		want     bool
	}{
		{"a lost answer presented again minutes later", func(t *testing.T, f *rotationFixture) bool {
			if _, err := f.svc.Refresh(context.Background(), f.token, "ua", "ip"); err != nil {
				t.Fatalf("the rotation whose answer is lost: %v", err)
			}
			f.clock.Advance(13 * time.Minute)
			_, err := f.svc.Refresh(context.Background(), f.token, "ua", "ip")
			return err == nil
		}, false},
		{"a tab presenting after another tab's needless rotation", func(t *testing.T, f *rotationFixture) bool {
			t1, err := f.svc.Refresh(context.Background(), f.token, "tab-1", "ip")
			if err != nil {
				t.Fatalf("tab 1's rotation: %v", err)
			}
			if _, err := f.svc.Refresh(context.Background(), t1.RefreshToken, "tab-2", "ip"); err != nil {
				t.Fatalf("tab 2's needless rotation: %v", err)
			}
			f.clock.Advance(time.Second)
			_, err = f.svc.Refresh(context.Background(), f.token, "tab-3", "ip")
			return err == nil
		}, false},
		{"a check that failed after the successor's write", func(t *testing.T, f *rotationFixture) bool {
			f.sessions.failCheckArmed.Store(true)
			if _, err := f.svc.Refresh(context.Background(), f.token, "ua", "ip"); err == nil {
				t.Fatal("the refresh whose check failed reported success")
			}
			f.clock.Advance(time.Minute)
			_, err := f.svc.Refresh(context.Background(), f.token, "ua", "ip")
			return err == nil
		}, true},
		{"a client that gave up after the rotation of a stalled store", func(t *testing.T, f *rotationFixture) bool {
			ctx, cancel := context.WithCancel(context.Background())
			f.sessions.runBeforeNextCheck(cancel) // the client leaves while the check runs
			if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil {
				t.Fatal("the refresh its client gave up on reported success")
			}
			f.clock.Advance(2 * time.Minute)
			_, err := f.svc.Refresh(context.Background(), f.token, "ua", "ip")
			return err == nil
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t, errStoreUnreachable)
			if got := tc.straggle(t, f); got != tc.want {
				t.Fatalf("the straggler's own lineage survived: %t, want %t", got, tc.want)
			}
			if f.otherDeviceRevoked(t) {
				t.Fatal("a legitimate straggler revoked the account's other device")
			}
			if _, err := f.svc.Refresh(context.Background(), f.other, "other-device", "ip2"); err != nil {
				t.Fatalf("the other device no longer refreshes: %v", err)
			}
		})
	}
}
