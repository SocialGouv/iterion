package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A successor is handed out by the refresh that claims it (ClaimSession), and
// no refresh deletes a session it wrote. The cases below drive a refresh W
// whose own steps fail after another presentation P of the same token was
// handed the successor W wrote — two tabs at the access cookie's expiry, or
// one desktop retrying at once a presentation it abandoned: P's session must
// stay what it was handed.

// presentAgain runs P, another presentation of raw, and returns its answer.
func presentAgain(t *testing.T, f *rotationFixture, raw string) LoginResult {
	t.Helper()
	res, err := f.svc.Refresh(context.Background(), raw, "ua", "ip")
	if err != nil {
		t.Errorf("presentation P: %v", err)
	}
	return res
}

// mustStayHandedOut fails the test unless the session of p's token is live,
// claimed, and refreshes.
func mustStayHandedOut(t *testing.T, f *rotationFixture, p LoginResult) {
	t.Helper()
	if p.RefreshToken == "" {
		t.Fatal("presentation P was handed no successor: the witness did not drive the race it pins")
	}
	got := f.session(t, p.RefreshToken)
	if got.RevokedAt != nil || got.DeliveredAt == nil {
		t.Fatalf("the successor P was handed reads %+v: want live and claimed", got)
	}
	if _, err := f.svc.Refresh(context.Background(), p.RefreshToken, "ua", "ip"); err != nil {
		t.Fatalf("the successor P was handed no longer refreshes: %v", err)
	}
}

func TestRefresh_ASuccessorHandedOutStaysWhateverItsWriterMeetsAfter(t *testing.T) {
	t.Run("the writer's check fails after P was handed the row", func(t *testing.T) {
		f := newRotationFixture(t, errStoreUnreachable)
		var p LoginResult
		f.sessions.runBeforeNextCheck(func() {
			p = presentAgain(t, f, f.token)
			f.sessions.failCheckArmed.Store(true)
		})
		if _, err := f.svc.Refresh(context.Background(), f.token, "ua", "ip"); err == nil || errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("writer W: got %v, want its check's failure", err)
		}
		mustStayHandedOut(t, f, p)
	})
	t.Run("the writer's client leaves during its check", func(t *testing.T) {
		f := newRotationFixture(t, errStoreUnreachable)
		wctx, leave := context.WithCancel(context.Background())
		var p LoginResult
		f.sessions.runBeforeNextCheck(func() {
			p = presentAgain(t, f, f.token)
			leave()
		})
		if _, err := f.svc.Refresh(wctx, f.token, "ua", "ip"); !errors.Is(err, context.Canceled) {
			t.Fatalf("writer W: got %v, want context.Canceled", err)
		}
		mustStayHandedOut(t, f, p)
	})
	t.Run("the writer's write landed, P was handed it, then the write's answer was lost", func(t *testing.T) {
		f := newRotationFixture(t, errStoreUnreachable)
		var p LoginResult
		f.sessions.landThenFailCreateArmed.Store(true)
		f.sessions.runAfterNextCreate(func() { p = presentAgain(t, f, f.token) })
		if _, err := f.svc.Refresh(context.Background(), f.token, "ua", "ip"); err == nil {
			t.Fatal("writer W whose write answered an error reported success")
		}
		mustStayHandedOut(t, f, p)
	})
}

// TestRefresh_TwoCompletionsOfOneSuccessorAreHandedTheSameRow: the rotation's
// successor was never written; X completes it and leaves during its check,
// while Y — whose own write met X's row — is handed that row. Y's session
// must stay, and X's departure must not refuse Y.
func TestRefresh_TwoCompletionsOfOneSuccessorAreHandedTheSameRow(t *testing.T) {
	f := newRotationFixture(t, errStoreUnreachable)
	rotateLosingTheSuccessor(t, f, "ua", "ip")
	xctx, leave := context.WithCancel(context.Background())
	var y LoginResult
	f.sessions.runBeforeNextCheck(func() {
		y = presentAgain(t, f, f.token)
		leave()
	})
	if _, err := f.svc.Refresh(xctx, f.token, "ua", "ip"); !errors.Is(err, context.Canceled) {
		t.Fatalf("completion X: got %v, want context.Canceled", err)
	}
	if n := len(successorsOf(t, f, f.token)); n != 1 {
		t.Fatalf("two completions of one successor wrote %d row(s), want 1", n)
	}
	mustStayHandedOut(t, f, y)
}

// TestLogout_ASuccessorHandedOutWhileItsWriterFailedIsSignedOut: the user
// signs out with the successor P was handed while W failed. The sign-out
// must find its session and end the lineage, so a thief holding the retired
// token completes nothing after it.
func TestLogout_ASuccessorHandedOutWhileItsWriterFailedIsSignedOut(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	var p LoginResult
	f.sessions.runBeforeNextCheck(func() {
		p = presentAgain(t, f, f.token)
		f.sessions.failCheckArmed.Store(true)
	})
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil {
		t.Fatal("writer W whose check failed reported success")
	}
	if p.RefreshToken == "" {
		t.Fatal("presentation P was handed no successor: the witness did not drive the race it pins")
	}
	if err := f.svc.Logout(ctx, p.RefreshToken); err != nil {
		t.Fatalf("the sign-out with P's token: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, f.token, "thief", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the retired token presented after the sign-out: got %v, want ErrSessionRevoked", err)
	}
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("%d live session(s) in the signed-out lineage", n)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("the sign-out revoked the account's other device")
	}
}

// TestRefresh_ASuccessorHandedToNobodyIsCompletedWithinTheHorizonOnly: a
// successor written and never claimed is completed like one never written —
// past the reuse window, inside the completion horizon — and reads as reuse
// past the horizon, revoking its lineage and nothing else.
func TestRefresh_ASuccessorHandedToNobodyIsCompletedWithinTheHorizonOnly(t *testing.T) {
	ctx := context.Background()
	t.Run("past the window, inside the horizon", func(t *testing.T) {
		f := newRotationFixture(t, errStoreUnreachable)
		f.sessions.failCheckArmed.Store(true)
		if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil {
			t.Fatal("the refresh whose check failed reported success")
		}
		f.clock.Advance(completionHorizon - time.Second)
		res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
		if err != nil {
			t.Fatalf("an unclaimed successor presented inside the horizon: %v", err)
		}
		if got := f.session(t, res.RefreshToken); got.DeliveredAt == nil || got.RevokedAt != nil {
			t.Fatalf("the completion handed out %+v, want live and claimed", got)
		}
	})
	t.Run("past the horizon", func(t *testing.T) {
		f := newRotationFixture(t, errStoreUnreachable)
		f.sessions.failCheckArmed.Store(true)
		if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil {
			t.Fatal("the refresh whose check failed reported success")
		}
		f.clock.Advance(completionHorizon + time.Second)
		if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("an unclaimed successor presented past the horizon: got %v, want ErrSessionRevoked", err)
		}
		if n := f.liveInLineage(t, f.token); n != 0 {
			t.Fatalf("%d live session(s) in a lineage whose token came back past the horizon", n)
		}
		if f.otherDeviceRevoked(t) {
			t.Fatal("the reuse revoked the account's other device")
		}
	})
}

// TestRefresh_AClaimWhoseAnswerWasLostIsRedeliveredWithinTheWindowOnly: a
// claim is the server's last word before it answers, not proof the client
// read the answer. A claimed successor whose answer was lost is the window's
// case: redelivered inside it, reuse past it.
func TestRefresh_AClaimWhoseAnswerWasLostIsRedeliveredWithinTheWindowOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		advance time.Duration
		want    error
	}{
		{"inside the window", DefaultRefreshReuseGrace - time.Second, nil},
		{"past the window", DefaultRefreshReuseGrace + time.Second, ErrSessionRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t, errStoreUnreachable)
			f.sessions.landThenFailClaimArmed.Store(true)
			if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); err == nil {
				t.Fatal("the refresh whose claim answered an error reported success")
			}
			written := successorsOf(t, f, f.token)
			if len(written) != 1 || written[0].DeliveredAt == nil {
				t.Fatalf("after the claim's lost answer the successors read %+v, want one, claimed", written)
			}
			f.clock.Advance(tc.advance)
			_, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
			if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("the re-presentation %s: got %v, want %v", tc.name, err, tc.want)
			}
		})
	}
}

// TestRefresh_ARevocationBetweenTheCheckAndTheClaimRefusesTheDelivery: a
// sign-out of the lineage lands after the successor's check found no record
// and before its claim. The claim matches nothing — the pass revoked the row
// — so the presentation is refused, and the lineage holds no live session.
func TestRefresh_ARevocationBetweenTheCheckAndTheClaimRefusesTheDelivery(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.runBeforeNextClaim(func() {
		if err := f.svc.Logout(context.Background(), f.token); err != nil {
			t.Errorf("the sign-out: %v", err)
		}
	})
	res, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a delivery a sign-out overtook between its check and its claim: got %v (a session delivered: %t), want ErrSessionRevoked", err, res.RefreshToken != "")
	}
	if n := f.liveInLineage(t, f.token); n != 0 {
		t.Fatalf("%d live session(s) in the signed-out lineage", n)
	}
}
