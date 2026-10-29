package server

import (
	"context"
	"strings"
	"sync"
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

// patOpenedSocket wires the fixture with the given personal-access-token
// store, puts a row for u-member-a pinned to tenant-A in it, and opens the
// run stream with a ticket minted from that token — the way the studio's
// pane reaches a WebSocket when the CLI's credential is a PAT. The row is
// usable when the socket opens: what each witness does to it afterwards is
// the EVENT it judges, never a deadline the setup must beat.
func patOpenedSocket(t *testing.T, pats pat.Store) (*Server, string, *websocket.Conn) {
	t.Helper()
	t.Setenv("ITERION_RUN_WS_STANDING_TICK", "50ms")
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	srv.pats = pats
	srv.wirePATState()
	row := pat.Token{
		ID:        "pat-1",
		UserID:    "u-member-a",
		Name:      "ci",
		TeamID:    "tenant-A",
		CreatedAt: time.Now().UTC(),
	}
	_, hash, last4, fp, err := pat.MintToken()
	if err != nil {
		t.Fatalf("mint the token material: %v", err)
	}
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
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"}); err != nil {
		t.Fatal(err)
	}
	awaitEnvelope(t, conn, 10*time.Second, wsTypeSnapshot)
	return srv, row.ID, conn
}

// expiringPATs answers the row the store holds with its expiry moved to
// whatever expire() last named. The row's expiry is what an open socket
// re-reads per tick (auth.PATState → pat.Token.Usable), so the credential
// expires when the TEST says so: a deadline set before the stack is built,
// the dial, the subscribe and the snapshot is a race the machine arbitrates,
// and a slow enough machine makes it expire before the socket is even up.
type expiringPATs struct {
	*pat.MemoryStore
	mu      sync.Mutex
	expired time.Time
}

func (e *expiringPATs) Get(ctx context.Context, id string) (pat.Token, error) {
	tk, err := e.MemoryStore.Get(ctx, id)
	if err != nil {
		return tk, err
	}
	e.mu.Lock()
	at := e.expired
	e.mu.Unlock()
	if !at.IsZero() {
		tk.ExpiresAt = &at
	}
	return tk, nil
}

// expire makes every later read of the row answer an expiry at at.
func (e *expiringPATs) expire(at time.Time) {
	e.mu.Lock()
	e.expired = at
	e.mu.Unlock()
}

// TestRunWS_ThePassiveStreamDiesWithThePATsRevocation: the token's row is
// re-read on every tick, so a revoked personal access token's socket closes
// — the ticket outlives nothing of its minter.
func TestRunWS_ThePassiveStreamDiesWithThePATsRevocation(t *testing.T) {
	pats := pat.NewMemoryStore()
	_, patID, conn := patOpenedSocket(t, pats)
	if err := pats.Revoke(context.Background(), patID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke the PAT: %v", err)
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.ClosePolicyViolation || !strings.HasPrefix(ce.Text, wsCloseCredentialRefused) {
		t.Fatalf("close = %d %q, want %d with a reason starting %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, wsCloseCredentialRefused)
	}
}

// TestRunWS_ThePassiveStreamDiesAtThePATsExpiry: a personal access token's
// expiry is read from its row on every tick, so the socket closes when the
// credential expires — not at the stream's lifetime cap. (An access token's
// own expiry is the documented exception: the client renews its session over
// HTTP while the socket stays open — auth.StandingOfOpenSocket.)
//
// The expiry is the EXPIRY and not the revocation: the row is never revoked
// here, which is what tells this witness apart from its sibling above. Both
// refusals read the same row, so only this separation proves the expiry is
// read at all.
func TestRunWS_ThePassiveStreamDiesAtThePATsExpiry(t *testing.T) {
	pats := &expiringPATs{MemoryStore: pat.NewMemoryStore()}
	_, patID, conn := patOpenedSocket(t, pats)
	pats.expire(time.Now().Add(-time.Second).UTC())
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.ClosePolicyViolation || !strings.HasPrefix(ce.Text, wsCloseCredentialRefused) {
		t.Fatalf("close = %d %q, want %d with a reason starting %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, wsCloseCredentialRefused)
	}
	row, err := pats.MemoryStore.Get(context.Background(), patID)
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if row.RevokedAt != nil {
		t.Fatal("the row was revoked, so this witness proves the revocation its sibling already covers, not the expiry")
	}
}
