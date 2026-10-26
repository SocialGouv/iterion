package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// wsTicketGate is the in-memory WS-ticket store with a one-shot failure of
// the redemption, the way a saturated pool fails the first read of an
// upgrade. The failure happens before the store is reached, so the ticket
// stays live — as it may after a FindOneAndDelete whose answer was lost.
type wsTicketGate struct {
	*wsticket.MemoryStore
	failRedeemArmed atomic.Bool
}

func (g *wsTicketGate) Redeem(ctx context.Context, ticket string) (auth.Identity, error) {
	if g.failRedeemArmed.CompareAndSwap(true, false) {
		return auth.Identity{}, fmt.Errorf("wsticket: redeem: %w", errClusterUnreachable)
	}
	return g.MemoryStore.Redeem(ctx, ticket)
}

// wsTicketFixture is a real HTTP server — the full handler, so a dial goes
// through authMiddleware and requireAuth — on a gated WS-ticket store, with
// its log captured.
type wsTicketFixture struct {
	tickets *wsTicketGate
	logs    *syncBuffer
	base    string // ws://host:port
}

func newWSTicketFixture(t *testing.T) *wsTicketFixture {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	f := &wsTicketFixture{
		tickets: &wsTicketGate{MemoryStore: wsticket.NewMemoryStore(time.Minute)},
		logs:    &syncBuffer{},
	}
	// The account behind the tickets: a redemption asks the minting
	// credential's account again (auth.Service.CheckDerivation).
	ids := identity.NewMemoryStore()
	if _, err := ids.CreateUser(context.Background(), identity.User{ID: "u1", Email: "u1@example.com", Status: identity.UserStatusActive}); err != nil {
		t.Fatalf("seed the account: %v", err)
	}
	svc, err := auth.NewService(auth.Config{Store: ids, Sessions: auth.NewMemorySessionStore(), Signer: signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	s := New(Config{
		WorkDir: t.TempDir(), Bind: "127.0.0.1", SkipProjectRegistration: true,
		AuthSigner: signer, AuthService: svc, WSTickets: f.tickets,
	}, iterlog.New(iterlog.LevelError, f.logs))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	f.base = "ws" + strings.TrimPrefix(srv.URL, "http")
	return f
}

// mint stores a ticket for id the way POST /api/ws/ticket does.
func (f *wsTicketFixture) mint(t *testing.T, id auth.Identity) string {
	t.Helper()
	ticket, err := f.tickets.Mint(context.Background(), id)
	if err != nil {
		t.Fatalf("mint a ws ticket: %v", err)
	}
	return ticket
}

// dial opens path?ticket=<ticket> with a WebSocket handshake, as the studio
// and the desktop pane do, and answers the handshake's status and body. A
// socket it opens is closed at once.
func (f *wsTicketFixture) dial(t *testing.T, path, ticket string) (int, string) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(f.base+path+"?ticket="+url.QueryEscape(ticket), nil)
	if err == nil {
		_ = conn.Close()
		return resp.StatusCode, ""
	}
	if resp == nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// TestRequireAuth_AWSTicketRedemptionThatFailsIsAFailure: a ?ticket= on a WS
// path is a credential, and a redemption the ticket store could not COMPLETE
// is the server's failure, not a verdict on it. Each WS-ticket path answers
// 500 with the generic body and the cause in the server log — never a 401
// saying the ticket is dead, and never the ticket in the log: the store may
// not have consumed it, and the same ticket upgrades once the store answers.
func TestRequireAuth_AWSTicketRedemptionThatFailsIsAFailure(t *testing.T) {
	for _, path := range []string{"/api/ws", "/api/runs/run-1/browser/cdp"} {
		t.Run(path, func(t *testing.T) {
			f := newWSTicketFixture(t)
			ticket := f.mint(t, auth.Identity{UserID: "u1", Email: "u1@example.com", TeamID: "t1", Role: identity.RoleMember, Credential: auth.CredentialAccessToken})
			f.tickets.failRedeemArmed.Store(true)
			code, body := f.dial(t, path, ticket)
			if code != http.StatusInternalServerError {
				t.Fatalf("a ticket redemption the store could not complete answered %d %s, want 500", code, body)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(body), &got); err != nil || got["error"] != identityUnavailable || got["request_id"] == "" || len(got) != 2+boolInt(got["trace_id"] != "") {
				t.Fatalf("a failed ticket redemption answered the body %s, want {\"error\":%q}", body, identityUnavailable)
			}
			if strings.Contains(body, internalHost) {
				t.Fatalf("a failed ticket redemption leaked the store's internal host to the client: %s", body)
			}
			logged := f.logs.String()
			if !strings.Contains(logged, internalHost) {
				t.Fatalf("a failed ticket redemption is not in the server log: %q", logged)
			}
			if strings.Contains(logged, ticket) {
				t.Fatalf("the server log carries the ticket, which the store may not have consumed: %q", logged)
			}
			if path != "/api/ws" {
				return
			}
			if code, body := f.dial(t, path, ticket); code != http.StatusSwitchingProtocols {
				t.Fatalf("the ticket presented again once the store answers got %d %s, want the upgrade", code, body)
			}
		})
	}
}

// TestRequireAuth_AWSTicketTheStoreRefusesIsARefusal: a ticket the store
// refuses — unknown, already redeemed — and one that names no user answer
// 401 and write nothing to the server log. A presented ticket is never a
// fall-through to the bearer paths.
func TestRequireAuth_AWSTicketTheStoreRefusesIsARefusal(t *testing.T) {
	f := newWSTicketFixture(t)
	used := f.mint(t, auth.Identity{UserID: "u1", Email: "u1@example.com", TeamID: "t1", Role: identity.RoleMember, Credential: auth.CredentialAccessToken})
	if code, body := f.dial(t, "/api/ws", used); code != http.StatusSwitchingProtocols {
		t.Fatalf("a live ticket got %d %s, want the upgrade", code, body)
	}
	nobody := f.mint(t, auth.Identity{TeamID: "t1", Role: identity.RoleMember, Credential: auth.CredentialAccessToken})
	for _, tc := range []struct{ name, ticket string }{
		{"an unknown ticket", "not-a-ticket"},
		{"an already redeemed ticket", used},
		{"a ticket that names no user", nobody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.logs.String()
			code, body := f.dial(t, "/api/ws", tc.ticket)
			var got map[string]string
			if code != http.StatusUnauthorized || json.Unmarshal([]byte(body), &got) != nil || got["error"] != "invalid or expired ws ticket" || len(got) != 1 {
				t.Fatalf("%s answered %d %s, want 401 {\"error\":\"invalid or expired ws ticket\"}", tc.name, code, body)
			}
			if logged := strings.TrimPrefix(f.logs.String(), before); logged != "" {
				t.Fatalf("a refused ticket wrote to the server log: %q", logged)
			}
		})
	}
}
