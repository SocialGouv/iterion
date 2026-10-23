package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
)

// standingStack parks run-1 (tenant-A) in a status a member may cancel and
// serves the production middleware chain in front of the mux, so an open
// socket carries the identity requireAuth stamped — the snapshot every
// standing question is asked about.
func standingStack(t *testing.T, status store.RunStatus) (*Server, *httptest.Server, *tenantGuardStore) {
	t.Helper()
	srv, guard := newRunScopeServer(t)
	ts := fullStack(t, srv)
	if err := guard.UpdateRunStatus(store.WithTenant(context.Background(), "tenant-A"), "run-1", status, ""); err != nil {
		t.Fatalf("park the run: %v", err)
	}
	return srv, ts, guard
}

// dialWithTicket opens path with a single-use ticket minted for id, the way
// the studio's desktop pane and the cloud SPA reach a WebSocket.
func dialWithTicket(t *testing.T, srv *Server, ts *httptest.Server, path string, id auth.Identity) *websocket.Conn {
	t.Helper()
	ticket, err := srv.wsTickets.Mint(context.Background(), id)
	if err != nil {
		t.Fatalf("mint a ws ticket: %v", err)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	target := "ws" + strings.TrimPrefix(ts.URL, "http") + path + sep + "ticket=" + url.QueryEscape(ticket)
	conn, resp, err := websocket.DefaultDialer.Dial(target, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (%d)", path, err, code)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// awaitWSClose reads until the peer's close frame and answers it, with every
// text frame it passed on the way (the shell's typed error rides one).
func awaitWSClose(t *testing.T, c *websocket.Conn, within time.Duration) (*websocket.CloseError, []string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	var texts []string
	for {
		kind, raw, err := c.ReadMessage()
		if err == nil {
			if kind == websocket.TextMessage {
				texts = append(texts, string(raw))
			}
			continue
		}
		var ce *websocket.CloseError
		if errors.As(err, &ce) {
			return ce, texts
		}
		t.Fatalf("read until the close frame: %v (text frames seen: %v)", err, texts)
		return nil, nil
	}
}

// awaitEnvelope reads run-socket envelopes until one of the wanted types.
func awaitEnvelope(t *testing.T, c *websocket.Conn, within time.Duration, want ...string) runWSEnvelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	for {
		var env runWSEnvelope
		if err := c.ReadJSON(&env); err != nil {
			t.Fatalf("read an envelope of %v: %v", want, err)
		}
		for _, w := range want {
			if env.Type == w {
				return env
			}
		}
	}
}

func errorCodeOf(t *testing.T, env runWSEnvelope) wsErrorPayload {
	t.Helper()
	var p wsErrorPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("decode the error payload %s: %v", string(env.Payload), err)
	}
	return p
}

func runStatusOf(t *testing.T, srv *Server, runID string) store.RunStatus {
	t.Helper()
	run, err := srv.runs.LoadRunCtx(store.WithTenant(context.Background(), "tenant-A"), runID)
	if err != nil {
		t.Fatalf("load %s: %v", runID, err)
	}
	return run.Status
}

func disableAccount(t *testing.T, srv *Server, userID string) {
	t.Helper()
	disabled := identity.UserStatusDisabled
	if _, err := srv.authStore().PatchUser(context.Background(), userID, identity.UserPatch{Status: &disabled}); err != nil {
		t.Fatalf("disable %s: %v", userID, err)
	}
}

// A mutating envelope asks the standing of the credential the socket was
// opened with: the account disabled after the upgrade, the cancel is refused,
// the run is NOT cancelled, and the socket closes naming the refusal — the
// identity is snapshotted at the upgrade, so nothing else would have noticed.
func TestRunWS_ADisabledAccountsMutatingEnvelopeIsRefusedAndTheSocketCloses(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", caller("u-member-a", "tenant-A"))

	disableAccount(t, srv, "u-member-a")

	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}); err != nil {
		t.Fatalf("write the cancel: %v", err)
	}
	env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError)
	if env.Type != wsTypeError {
		t.Fatalf("the cancel of a disabled account answered %q, want an error", env.Type)
	}
	if got := errorCodeOf(t, env).Code; got != wsCloseCredentialRefused {
		t.Fatalf("error code = %q, want %q", got, wsCloseCredentialRefused)
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.ClosePolicyViolation {
		t.Fatalf("close code = %d, want %d (policy violation)", ce.Code, websocket.ClosePolicyViolation)
	}
	if ce.Text != expectedCloseReason(wsCloseCredentialRefused, auth.ErrAccountDisabled.Error()) {
		t.Fatalf("close reason = %q, want exactly %q", ce.Text, expectedCloseReason(wsCloseCredentialRefused, auth.ErrAccountDisabled.Error()))
	}
	if got := runStatusOf(t, srv, "run-1"); got != store.RunStatusPausedWaitingHuman {
		t.Fatalf("run status = %q, want it untouched: a refused cancel cancels nothing", got)
	}
}

// The exact close reasons the wire carries. Every policy close composes its
// token with a cause (writeWSClose: token + ": " + cause), and every cause
// these surfaces send is a fixed string — so the witnesses assert the FULL
// reason, and a regression to the bare token, to the other cause, or to any
// rewording fails the witness that names it. The cut-reason witness
// (TestWSCloseReasonFitsAControlFrame) stays a prefix assertion on purpose:
// its scenario is the cut itself, of a reason deliberately over-long.
const (
	// The scope-lost causes runSocketStanding's two scope verdicts carry
	// (run_scope.go) — one per verdict, both always present.
	causeRunGone      = "the run this socket watches is gone"
	causeStandingGone = "your standing in the run's team no longer holds"
	// The standing-unavailable cause the bridge's three-read fail-closed
	// close carries (runs_browser.go).
	causeStandingUnreadable = "the standing of this connection could not be re-checked — reconnect"
	// The lifetime cause both run-socket surfaces close with.
	causeReopen = "reopen the socket to present a credential again"
)

// expectedCloseReason composes the full reason a close frame carries for
// token and cause, the way writeWSClose does.
func expectedCloseReason(token, cause string) string {
	if cause == "" {
		return token
	}
	return token + ": " + cause
}

// The other half of the same question: the account stays ACTIVE and its
// sessions are signed out everywhere. A credential that names a session is
// refused on the revocation alone, so neither guard rests on the other.
func TestRunWS_ARevokedSessionsMutatingEnvelopeIsRefusedAndTheSocketCloses(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	id := caller("u-member-a", "tenant-A")
	id.SessionLineage = "lineage-1"
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", id)

	if err := srv.authSvc.RevokeUserSessions(context.Background(), "u-member-a"); err != nil {
		t.Fatalf("sign out everywhere: %v", err)
	}
	user, err := srv.authStore().GetUser(context.Background(), "u-member-a")
	if err != nil {
		t.Fatalf("read the account: %v", err)
	}
	if user.Status != identity.UserStatusActive {
		t.Fatalf("the account is %q: this witness must rest on the REVOCATION, not on a status", user.Status)
	}

	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypePause, AckID: "p1"}); err != nil {
		t.Fatalf("write the pause: %v", err)
	}
	env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError)
	if got := errorCodeOf(t, env).Code; env.Type != wsTypeError || got != wsCloseCredentialRefused {
		t.Fatalf("the pause of a revoked session answered %q/%q, want an error %q", env.Type, got, wsCloseCredentialRefused)
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseCredentialRefused, auth.ErrSessionRevoked.Error())
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// The control: a credential that still stands acts, and the socket stays open
// — the standing question refuses nothing a live session sends.
func TestRunWS_ALiveSessionsMutatingEnvelopeStillActs(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", caller("u-member-a", "tenant-A"))

	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}); err != nil {
		t.Fatalf("write the cancel: %v", err)
	}
	env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError)
	if env.Type != wsTypeAck {
		t.Fatalf("a live session's cancel answered %q %s, want an ack", env.Type, string(env.Payload))
	}
	if got := runStatusOf(t, srv, "run-1"); got != store.RunStatusCancelled {
		t.Fatalf("run status = %q, want cancelled", got)
	}
	// Still open: the socket answers the heartbeat after the mutation.
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypePing, AckID: "ping"}); err != nil {
		t.Fatalf("write the ping: %v", err)
	}
	if got := awaitEnvelope(t, conn, 10*time.Second, wsTypePong, wsTypeError).Type; got != wsTypePong {
		t.Fatalf("the socket answered %q to a ping, want a pong: a live credential closes nothing", got)
	}
}

// The local run console: no auth context ever reached the handler, so there
// is no credential to ask about and the console keeps acting — the same
// carve-out the role gate beside it makes. This server has no auth service at
// all, so a question asked here would refuse every envelope.
func TestRunWS_ALocalConsoleWithNoCredentialStillActs(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-local", "wf", store.RunStatusPausedWaitingHuman)
	c := dialRunWS(t, hs, "run-local")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeCancel, AckID: "c1"})
	env := awaitEnvelope(t, c, 10*time.Second, wsTypeAck, wsTypeError)
	if env.Type != wsTypeAck {
		t.Fatalf("the local console's cancel answered %q %s, want an ack", env.Type, string(env.Payload))
	}
}

// flakyIdentityStore fails its account reads once failReads is set, so a
// witness can tell the server's own failure from a refusal. The switch is an
// atomic: the socket's goroutine reads it while the test flips it.
type flakyIdentityStore struct {
	identity.Store
	failReads atomic.Bool
}

func (s *flakyIdentityStore) GetUser(ctx context.Context, id string) (identity.User, error) {
	if s.failReads.Load() {
		return identity.User{}, errors.New("identity: the store did not answer")
	}
	return s.Store.GetUser(ctx, id)
}

// A standing read that did not complete refuses THAT envelope and keeps the
// socket: the server's own failure is no verdict on the credential, so a
// store blip must not sign a live operator out of their run console.
func TestRunWS_AStandingReadThatFailsRefusesTheEnvelopeAndKeepsTheSocket(t *testing.T) {
	srv, guard := newRunScopeServer(t)
	gate := &flakyIdentityStore{Store: srv.authStore()}
	svc, err := auth.NewService(auth.Config{
		Store:      gate,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     srv.signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service over the flaky store: %v", err)
	}
	srv.authSvc = svc
	ts := fullStack(t, srv)
	if err := guard.UpdateRunStatus(store.WithTenant(context.Background(), "tenant-A"), "run-1", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("park the run: %v", err)
	}
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", caller("u-member-a", "tenant-A"))

	gate.failReads.Store(true)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}); err != nil {
		t.Fatalf("write the cancel: %v", err)
	}
	env := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError)
	if got := errorCodeOf(t, env).Code; env.Type != wsTypeError || got != "standing_unavailable" {
		t.Fatalf("a failed standing read answered %q/%q, want an error standing_unavailable", env.Type, got)
	}
	if got := runStatusOf(t, srv, "run-1"); got != store.RunStatusPausedWaitingHuman {
		t.Fatalf("run status = %q, want it untouched: an unanswered question mutates nothing", got)
	}
	// The socket survives: the read recovers and the next envelope is served.
	gate.failReads.Store(false)
	if err := conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c2"}); err != nil {
		t.Fatalf("write the second cancel: %v", err)
	}
	if got := awaitEnvelope(t, conn, 10*time.Second, wsTypeAck, wsTypeError).Type; got != wsTypeAck {
		t.Fatalf("the retried cancel answered %q, want an ack: the socket was kept", got)
	}
}

// The absolute lifetime: a socket nobody closes stops by itself, naming the
// bound so its client redials instead of reading a transport failure. It is
// what bounds the read stream, which no envelope of its own passes through.
func TestRunWS_TheSocketStopsAtItsLifetimeCap(t *testing.T) {
	t.Setenv("ITERION_RUN_WS_MAX_LIFETIME", "200ms")
	srv, ts, _ := standingStack(t, store.RunStatusPausedWaitingHuman)
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", caller("u-member-a", "tenant-A"))

	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d, want %d (normal: redial)", ce.Code, websocket.CloseNormalClosure)
	}
	if ce.Text != expectedCloseReason(wsCloseLifetimeReached, causeReopen) {
		t.Fatalf("close reason = %q, want exactly %q", ce.Text, expectedCloseReason(wsCloseLifetimeReached, causeReopen))
	}
}

func TestRunWSMaxLifetimeEnvOverride(t *testing.T) {
	if runWSMaxLifetime() != defaultRunWSMaxLifetime {
		t.Fatalf("lifetime = %v with no override, want %v", runWSMaxLifetime(), defaultRunWSMaxLifetime)
	}
	t.Setenv("ITERION_RUN_WS_MAX_LIFETIME", "45m")
	if got := runWSMaxLifetime(); got != 45*time.Minute {
		t.Fatalf("lifetime = %v, want 45m", got)
	}
	// A value nobody can parse falls back to the default, never to zero.
	t.Setenv("ITERION_RUN_WS_MAX_LIFETIME", "bogus")
	if got := runWSMaxLifetime(); got != defaultRunWSMaxLifetime {
		t.Fatalf("lifetime = %v on a bogus override, want the default %v", got, defaultRunWSMaxLifetime)
	}
}

// A close reason longer than a control frame carries is cut to what fits and
// stays valid UTF-8: an over-long reason would otherwise be a frame the peer
// must fail, which would lose the close code the client reads.
func TestWSCloseReasonFitsAControlFrame(t *testing.T) {
	cause := strings.Repeat("é", 200) // 400 bytes, multi-byte on purpose
	served := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(served)
		if err := writeWSClose(conn, websocket.ClosePolicyViolation, wsCloseCredentialRefused, cause); err != nil {
			t.Errorf("writeWSClose: %v", err)
		}
	}))
	t.Cleanup(ts.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	<-served
	if ce.Code != websocket.ClosePolicyViolation {
		t.Fatalf("close code = %d, want %d", ce.Code, websocket.ClosePolicyViolation)
	}
	if len(ce.Text) > wsCloseReasonMax {
		t.Fatalf("close reason is %d bytes, want at most %d", len(ce.Text), wsCloseReasonMax)
	}
	if !utf8.ValidString(ce.Text) {
		t.Fatalf("close reason %q is not valid UTF-8", ce.Text)
	}
	if !strings.HasPrefix(ce.Text, wsCloseCredentialRefused) {
		t.Fatalf("close reason = %q, want the %q token first", ce.Text, wsCloseCredentialRefused)
	}
}

// The post-mortem shell: every input frame is a command in the run's
// worktree, so the credential it was opened with is asked again while it is
// open. A refusal ends the session, naming the bound.
func TestRunShell_ARefusedCredentialEndsTheOpenShell(t *testing.T) {
	t.Setenv("ITERION_RUN_SHELL_WATCH_PERIOD", "100ms")
	srv, ts, guard := standingStack(t, store.RunStatusPausedWaitingHuman)
	shellWorktree(t, guard, "run-1", t.TempDir())
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1/shell", caller("u-member-a", "tenant-A"))

	disableAccount(t, srv, "u-member-a")

	ce, texts := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseCredentialRefused, auth.ErrAccountDisabled.Error())
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
	if !containsSubstring(texts, wsCloseCredentialRefused) {
		t.Fatalf("control frames = %v, want one naming %q", texts, wsCloseCredentialRefused)
	}
}

// The control: a credential that still stands keeps the shell through several
// watchdog ticks.
func TestRunShell_ALiveCredentialKeepsTheOpenShell(t *testing.T) {
	t.Setenv("ITERION_RUN_SHELL_WATCH_PERIOD", "100ms")
	srv, ts, guard := standingStack(t, store.RunStatusPausedWaitingHuman)
	shellWorktree(t, guard, "run-1", t.TempDir())
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1/shell", caller("u-member-a", "tenant-A"))

	// Several ticks later the shell still answers its ping.
	time.Sleep(600 * time.Millisecond)
	if err := conn.WriteJSON(shellControlMsg{Type: "ping"}); err != nil {
		t.Fatalf("write the ping: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		kind, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("the shell closed on a credential that stands: %v", err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var msg shellControlMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Type == "pong" {
			return
		}
		if msg.Type == "error" {
			t.Fatalf("the shell errored on a credential that stands: %q", msg.Message)
		}
	}
	t.Fatalf("no pong from the shell within the deadline")
}

// shellWorktree gives the run a preserved worktree the shell may open in.
func shellWorktree(t *testing.T, guard *tenantGuardStore, runID, dir string) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "tenant-A")
	run, err := guard.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load %s: %v", runID, err)
	}
	run.WorkDir = dir
	if err := guard.SaveRun(ctx, run); err != nil {
		t.Fatalf("save %s: %v", runID, err)
	}
}

func containsSubstring(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

// attachCDP registers a browser session whose CDP pipe is an in-memory conn,
// so the bridge has something to pump without a Chromium.
func attachCDP(t *testing.T, srv *Server, runID, sessionID string) {
	t.Helper()
	reg := mcp.NewMemoryBrowserRegistry()
	chromium, bridge := net.Pipe()
	t.Cleanup(func() { _ = chromium.Close() })
	if err := reg.Attach(mcp.BrowserSession{SessionID: sessionID, RunID: runID, CDPConn: bridge}); err != nil {
		t.Fatalf("attach a browser session: %v", err)
	}
	srv.browserSessions = reg
}

// The CDP bridge drives the run's Chromium (navigate, evaluate, read the
// page) for as long as it is open, so it asks the standing of its credential
// on a tick: a refusal closes it.
func TestBrowserCDP_ARefusedCredentialClosesTheOpenBridge(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	attachCDP(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))

	disableAccount(t, srv, "u-member-a")

	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseCredentialRefused, auth.ErrAccountDisabled.Error())
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// The control: a credential that still stands keeps the bridge through
// several standing ticks.
func TestBrowserCDP_ALiveCredentialKeepsTheOpenBridge(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	attachCDP(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))

	time.Sleep(600 * time.Millisecond)
	// Nothing arrived and nothing closed: a read that times out is the proof
	// the bridge is still up (a closed one answers a close frame at once).
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		t.Fatalf("the bridge closed on a credential that stands: %d %q", ce.Code, ce.Text)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read = %v, want a timeout on a bridge that is still open", err)
	}
}

// standingFlakyStore fails the account read once armed — the read a bridge's
// standing tick asks.
type standingFlakyStore struct {
	identity.Store
	fail atomic.Bool
}

func (s *standingFlakyStore) GetUser(ctx context.Context, id string) (identity.User, error) {
	if s.fail.Load() {
		return identity.User{}, errors.New("identity: the store did not answer")
	}
	return s.Store.GetUser(ctx, id)
}

// TestBrowserCDP_AStandingReadItCannotCompleteClosesTheBridge: the bridge
// has no envelope to refuse, so a standing read that does not complete keeps
// it only while the failures are isolated — after three in a row it closes
// (standing_unavailable, 1008 like every other policy close on this
// surface) instead of driving the run's Chromium on a standing nobody could
// re-check.
func TestBrowserCDP_AStandingReadItCannotCompleteClosesTheBridge(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	gate := &standingFlakyStore{Store: srv.authStore()}
	svc, err := auth.NewService(auth.Config{Store: gate, Sessions: auth.NewMemorySessionStore(), Signer: srv.signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	srv.authSvc = svc
	attachCDP(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))
	gate.fail.Store(true)
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseStandingUnavailable, causeStandingUnreadable)
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// attachCDPPipe registers a browser session whose CDP pipe is an in-memory
// conn and returns the run's Chromium end, so a witness can observe what the
// bridge forwards into the run's browser.
func attachCDPPipe(t *testing.T, srv *Server, runID, sessionID string) net.Conn {
	t.Helper()
	reg := mcp.NewMemoryBrowserRegistry()
	chromium, bridge := net.Pipe()
	t.Cleanup(func() { _ = chromium.Close() })
	if err := reg.Attach(mcp.BrowserSession{SessionID: sessionID, RunID: runID, CDPConn: bridge}); err != nil {
		t.Fatalf("attach a browser session: %v", err)
	}
	srv.browserSessions = reg
	return chromium
}

// driveCDPCommand sends one CDP command through the bridge and reports
// whether the run's Chromium end received it: the bridge is a dumb pump, so
// a forwarded frame IS the live control.
func driveCDPCommand(t *testing.T, conn *websocket.Conn, chromium net.Conn, id int) error {
	t.Helper()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(`{"id":`+strconv.Itoa(id)+`,"method":"Target.getTargets"}`)); err != nil {
		return err
	}
	_ = chromium.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, err := chromium.Read(buf)
	if err != nil {
		return err
	}
	if !strings.Contains(string(buf[:n]), "Target.getTargets") {
		return errors.New("the pipe carried something else: " + string(buf[:n]))
	}
	return nil
}

// TestBrowserCDP_ARemovedMemberClosesAtItsNextStandingTick: the bridge is a
// live command surface (navigate, evaluate, read the page), so the scope its
// upgrade resolved is re-derived on the standing tick, the run console's own
// per-envelope computation — a member removed from the run's team stops
// driving the run's Chromium at the next tick, and the close names scope_lost.
func TestBrowserCDP_ARemovedMemberClosesAtItsNextStandingTick(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	chromium := attachCDPPipe(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))

	// Control: the member drives while they still belong.
	if err := driveCDPCommand(t, conn, chromium, 1); err != nil {
		t.Fatalf("control: the member's drive did not reach the run's Chromium: %v", err)
	}

	if err := srv.authStore().DeleteMembership(context.Background(), "u-member-a", "tenant-A"); err != nil {
		t.Fatalf("remove the membership: %v", err)
	}

	// A few standing periods later the drive no longer reaches the run's
	// Chromium, and the bridge is closed naming scope_lost.
	deadline := time.Now().Add(10 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		if err := driveCDPCommand(t, conn, chromium, 2); err != nil {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("a member removed from the run's team still drives the run's Chromium through the open bridge")
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseScopeLost, causeStandingGone)
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// TestBrowserCDP_ADemotedSuperAdminClosesAtItsNextStandingTick: the
// super-admin reach is read from the account as the tick's own standing read
// returned it, never from the flag the upgrade snapshotted — a super-admin
// demoted mid-bridge loses the reach the bridge was opened through.
func TestBrowserCDP_ADemotedSuperAdminClosesAtItsNextStandingTick(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	// sre holds no membership anywhere: run-1 was reachable through the
	// super-admin reach alone.
	id := caller("sre", "tenant-B")
	id.IsSuperAdmin = true
	chromium := attachCDPPipe(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", id)

	// Control: the reach drives while the account is still a super-admin.
	if err := driveCDPCommand(t, conn, chromium, 1); err != nil {
		t.Fatalf("control: the super-admin's drive did not reach the run's Chromium: %v", err)
	}

	no := false
	if _, err := srv.authStore().PatchUser(context.Background(), "sre", identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatalf("demote the account: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		if err := driveCDPCommand(t, conn, chromium, 2); err != nil {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("a demoted super-admin still drives the run's Chromium through the open bridge")
	}
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseScopeLost, causeStandingGone)
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// TestBrowserCDP_ADemotedMemberOfTheRunsTeamKeepsTheBridge: the demotion
// costs the account only its super-admin reach — the live flag from the
// standing read replaces the snapshot, and a member the run's team still
// holds drives on through ticks that ask the question again. The refusal is
// for the reach alone.
func TestBrowserCDP_ADemotedMemberOfTheRunsTeamKeepsTheBridge(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	yes, no := true, false
	if _, err := srv.authStore().PatchUser(context.Background(), "u-member-a", identity.UserPatch{IsSuperAdmin: &yes}); err != nil {
		t.Fatalf("raise the account: %v", err)
	}
	chromium := attachCDPPipe(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))

	if _, err := srv.authStore().PatchUser(context.Background(), "u-member-a", identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatalf("demote the account: %v", err)
	}

	// Several standing periods later the bridge still drives: the caller is
	// a member of the run's team as the store reads it now.
	for i := 0; i < 5; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := driveCDPCommand(t, conn, chromium, i+1); err != nil {
			t.Fatalf("a demoted member of the run's team lost the bridge at tick %d: %v", i+1, err)
		}
	}
	// And nothing closed: a read that times out is the proof (a closed
	// bridge answers a close frame at once).
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err := conn.ReadMessage()
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		t.Fatalf("the bridge closed on a demotion that cost no standing in the run's team: %d %q", ce.Code, ce.Text)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read = %v, want a timeout on a bridge that is still open", err)
	}
}

// TestBrowserCDP_AScopeReadThatFailsClosesTheBridgeFailClosed: the scope
// read a standing tick needs counts toward the fail-closed limit like any
// other read that does not complete — one isolated failure keeps the bridge
// driving, three consecutive ones close it (standing_unavailable).
func TestBrowserCDP_AScopeReadThatFailsClosesTheBridgeFailClosed(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	gate := &membershipFlakyStore{Store: srv.authStore()}
	svc, err := auth.NewService(auth.Config{Store: gate, Sessions: auth.NewMemorySessionStore(), Signer: srv.signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	srv.authSvc = svc
	chromium := attachCDPPipe(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))

	// One isolated unreadable tick keeps the bridge: after it recovers the
	// drive still reaches the run's Chromium.
	gate.fail.Store(true)
	time.Sleep(250 * time.Millisecond)
	gate.fail.Store(false)
	time.Sleep(120 * time.Millisecond)
	if err := driveCDPCommand(t, conn, chromium, 1); err != nil {
		t.Fatalf("the bridge did not survive one unreadable standing tick: %v", err)
	}

	// Consecutive unreadable ticks close it fail-closed.
	gate.fail.Store(true)
	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseStandingUnavailable, causeStandingUnreadable)
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// TestBrowserCDP_AGoneRunClosesTheBridgeAtItsNextStandingTick: the tick
// re-derives the run's team live — a run deleted under an open bridge is a
// scope the bridge can no longer hold, closed scope_lost with the gone run
// named in the cause.
func TestBrowserCDP_AGoneRunClosesTheBridgeAtItsNextStandingTick(t *testing.T) {
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "100ms")
	srv, ts, guard := standingStack(t, store.RunStatusRunning)
	attachCDP(t, srv, "run-1", "sess-1")
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))

	if err := guard.DeleteRun(context.Background(), "run-1"); err != nil {
		t.Fatalf("delete the run: %v", err)
	}

	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseScopeLost, causeRunGone)
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}

// TestRunShell_ARemovedMemberClosesAtItsNextWatchTick: the shell's watchdog
// re-derives the scope its upgrade resolved too — a member removed from the
// run's team loses the shell at the next tick, with an error control frame
// naming the bound and a close frame that names scope_lost.
func TestRunShell_ARemovedMemberClosesAtItsNextWatchTick(t *testing.T) {
	t.Setenv("ITERION_RUN_SHELL_WATCH_PERIOD", "100ms")
	srv, ts, guard := standingStack(t, store.RunStatusPausedWaitingHuman)
	shellWorktree(t, guard, "run-1", t.TempDir())
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1/shell", caller("u-member-a", "tenant-A"))

	if err := srv.authStore().DeleteMembership(context.Background(), "u-member-a", "tenant-A"); err != nil {
		t.Fatalf("remove the membership: %v", err)
	}

	ce, texts := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseScopeLost, causeStandingGone)
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
	if !containsSubstring(texts, expectedCloseReason(wsCloseScopeLost, causeStandingGone)) {
		t.Fatalf("control frames = %v, want one naming %q", texts, want)
	}
}

// TestRunShell_ADemotedSuperAdminClosesAtItsNextWatchTick: a super-admin
// demoted mid-shell loses the reach the shell was opened through — the live
// flag from the watch tick's own standing read decides, never the flag the
// upgrade snapshotted.
func TestRunShell_ADemotedSuperAdminClosesAtItsNextWatchTick(t *testing.T) {
	t.Setenv("ITERION_RUN_SHELL_WATCH_PERIOD", "100ms")
	srv, ts, guard := standingStack(t, store.RunStatusPausedWaitingHuman)
	shellWorktree(t, guard, "run-1", t.TempDir())
	// sre holds no membership anywhere: run-1 was reachable through the
	// super-admin reach alone.
	id := caller("sre", "tenant-B")
	id.IsSuperAdmin = true
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1/shell", id)

	no := false
	if _, err := srv.authStore().PatchUser(context.Background(), "sre", identity.UserPatch{IsSuperAdmin: &no}); err != nil {
		t.Fatalf("demote the account: %v", err)
	}

	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	want := expectedCloseReason(wsCloseScopeLost, causeStandingGone)
	if ce.Code != websocket.ClosePolicyViolation || ce.Text != want {
		t.Fatalf("close = %d %q, want %d with exactly %q", ce.Code, ce.Text, websocket.ClosePolicyViolation, want)
	}
}
