package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2687: an open WebSocket is asked again about the credential it was opened
// with — a ticket is checked at its redemption AND the open socket re-asks
// per tick, so a sign-out-everywhere or a personal access token's revocation
// or expiry ends the stream instead of leaving it to the lifetime cap. The
// mutating-envelope half and the shell's and the bridge's ticks are witnessed
// in ws_standing_test.go; these three pin the passive stream (subscribe and
// subscribe_logs carry no envelope of their own) and the personal access
// token's row, which is where a PAT's expiry and revocation are read.

// TestRunWS_ThePassiveStreamDiesWithTheSessionRevocation: a socket that only
// reads, opened before a sign-out-everywhere, keeps streaming nothing past
// the next standing tick — the close names credential_refused, not the lost
// scope: the account is ACTIVE and the membership untouched.
func TestRunWS_ThePassiveStreamDiesWithTheSessionRevocation(t *testing.T) {
	t.Setenv("ITERION_RUN_WS_STANDING_TICK", "50ms")
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	id := caller("u-member-a", "tenant-A")
	id.SessionLineage = "lineage-1"
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"}); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, conn, 10*time.Second, wsTypeSnapshot)

	if err := srv.authSvc.RevokeUserSessions(context.Background(), "u-member-a"); err != nil {
		t.Fatalf("sign out everywhere: %v", err)
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseCredentialRefused, auth.ErrSessionRevoked.Error())
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// patOpenedSocket wires the fixture with personal access tokens, puts a row
// for u-member-a pinned to tenant-A with the given expiry, and opens the run
// stream with a ticket minted from that token — the way the studio's pane
// reaches a WebSocket when the CLI's credential is a PAT.
func patOpenedSocket(t *testing.T, expiresAt *time.Time) (*Server, *pat.MemoryStore, string, *websocket.Conn) {
	t.Helper()
	t.Setenv("ITERION_RUN_WS_STANDING_TICK", "50ms")
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	pats := pat.NewMemoryStore()
	srv.pats = pats
	srv.wirePATState()
	row := pat.Token{
		ID:        "pat-1",
		UserID:    "u-member-a",
		Name:      "ci",
		TeamID:    "tenant-A",
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}
	plaintext, hash, last4, fp, err := pat.MintToken()
	if err != nil {
		t.Fatalf("mint the token material: %v", err)
	}
	_ = plaintext
	row.TokenHash, row.TokenLast4, row.Fingerprint = hash, last4, fp
	if err := pats.Create(context.Background(), row); err != nil {
		t.Fatalf("create the PAT row: %v", err)
	}
	id := auth.Identity{
		UserID:       "u-member-a",
		Email:        "u-member-a@x",
		TeamID:       "tenant-A",
		Role:         identity.RoleMember,
		Credential:   auth.CredentialPAT,
		CredentialID: row.ID,
		TeamPin:      "tenant-A",
	}
	if expiresAt != nil {
		id.ExpiresAt = expiresAt.UTC()
	}
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"}); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, conn, 10*time.Second, wsTypeSnapshot)
	return srv, pats, row.ID, conn
}

// TestRunWS_ThePassiveStreamDiesWithThePATsRevocation: the token's row is
// re-read on every tick, so a revoked personal access token's socket closes
// — the ticket outlives nothing of its minter.
func TestRunWS_ThePassiveStreamDiesWithThePATsRevocation(t *testing.T) {
	_, pats, patID, conn := patOpenedSocket(t, nil)
	if err := pats.Revoke(context.Background(), patID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke the PAT: %v", err)
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.ClosePolicyViolation || !strings.HasPrefix(ce.Text, wsCloseCredentialRefused) {
		t.Fatalf("close = %d %q, want %d with a reason starting %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, wsCloseCredentialRefused)
	}
}

// TestRunWS_ThePassiveStreamDiesAtThePATsExpiry: a personal access token's
// expiry is read from its row on every tick, so the socket closes AT the
// credential's expiry — not at the stream's lifetime cap. (An access token's
// own expiry is the documented exception: the client renews its session over
// HTTP while the socket stays open — auth.StandingOfOpenSocket.)
func TestRunWS_ThePassiveStreamDiesAtThePATsExpiry(t *testing.T) {
	expires := time.Now().Add(300 * time.Millisecond).UTC()
	_, _, _, conn := patOpenedSocket(t, &expires)
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.ClosePolicyViolation || !strings.HasPrefix(ce.Text, wsCloseCredentialRefused) {
		t.Fatalf("close = %d %q, want %d with a reason starting %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, wsCloseCredentialRefused)
	}
	if elapsed := time.Since(expires); elapsed > 5*time.Second {
		t.Fatalf("the socket closed %s past the credential's expiry — a tick is 50ms", elapsed)
	}
}
