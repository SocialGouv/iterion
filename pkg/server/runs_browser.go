package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
)

// browserCDPReadDeadline is the per-frame read budget on the studio →
// CDP direction. Editor inputs (mouse/keyboard) are tiny and bursty;
// a generous 60s window keeps a quiet idle pane from being torn down.
const browserCDPReadDeadline = 60 * time.Second

// browserCDPWriteDeadline bounds how long a single frame can sit in
// the OS send buffer before we declare the client gone. Screencast
// frames at 10fps × ~100KB push back-pressure quickly when the
// receiver is paused, so this cannot be too long.
const browserCDPWriteDeadline = 5 * time.Second

// browserCDPMaxFrame caps the inbound size from the studio — input
// events should never exceed a few hundred bytes, so 64 KiB is more
// than generous and protects against accidental flooding.
const browserCDPMaxFrame = 64 * 1024

// defaultBrowserCDPStandingPeriod is how often an open bridge re-asks whether
// the credential it was opened with still stands. The bridge drives the run's
// Chromium (navigate, evaluate, read the page), so the question the upgrade
// asked is owed again while it is open; one store read per period per
// connection bounds its cost. A tick rather than a per-frame check because
// the bridge is a dumb pump whose inbound frames are mouse moves at
// screencast rate.
const defaultBrowserCDPStandingPeriod = 30 * time.Second

// browserCDPStandingTimeout bounds that read.
const browserCDPStandingTimeout = 10 * time.Second

// browserCDPStandingFailureLimit is how many consecutive standing reads that
// did not complete close an open CDP bridge. The bridge has no envelope to
// refuse, so a read the server could not complete would otherwise drive the
// run's Chromium indefinitely; after this many, the bridge closes and the
// studio reconnects (a fresh upgrade re-judges the credential).
const browserCDPStandingFailureLimit = 3

// wsCloseStandingUnavailable names that close on the wire.
const wsCloseStandingUnavailable = "standing_unavailable"

func browserCDPStandingPeriod() time.Duration {
	return envDurationOrDefault("ITERION_RUN_BROWSER_STANDING_PERIOD", defaultBrowserCDPStandingPeriod)
}

// handleBrowserCDP serves GET /api/runs/{id}/browser/cdp?session=<id>.
// It bridges the studio's WebSocket to the in-process Chromium CDP
// pipe registered in the BrowserRegistry. CDP frames are JSON-RPC
// strings, but the proxy is a dumb binary pump — we never parse the
// payload, so a future wire-format upgrade upstream is invisible
// here.
//
// Auth: same Origin allowlist as the run console WS. Future PRs will
// add a per-run capability token so the studio can dial without
// the SPA's full session.
//
// Cloud / desktop / web parity: this code path is identical across
// surfaces. Cloud k8s runs Chromium in the same pod as the worker
// (loopback transport), the desktop sandbox uses `docker exec
// --remote-debugging-pipe` on the run's container — the
// ChromiumRunner abstraction hides both.
func (s *Server) handleBrowserCDP(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if runID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "run id required")
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "session query parameter required")
		return
	}
	if s.runs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run console disabled")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), runID); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if s.browserSessions == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "browser registry not configured")
		return
	}
	sess, ok := s.browserSessions.Get(runID, sessionID)
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "browser session not found")
		return
	}
	if sess.CDPConn == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "browser session has no CDP pipe")
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// upgrader.Upgrade already wrote the HTTP error response.
		return
	}
	defer conn.Close()
	// This proxy is the session's only consumer (handleBrowserAttach spawns
	// a fresh Chromium per attach; nothing else reconnects to it), so once
	// the WS pump tears down there's nobody left to serve — Detach closes
	// the CDP pipe and unblocks/frees the pump goroutine still parked in
	// sess.CDPConn.Read, and its subprocess. Without this, every browser
	// pane disconnect leaked the Chromium process and that goroutine for
	// the life of the server.
	defer func() { _ = s.browserSessions.Detach(runID, sessionID) }()
	conn.SetReadLimit(browserCDPMaxFrame)

	// Both pumps share an err channel; the first one to drain
	// shuts the other down.
	var once sync.Once
	stop := make(chan struct{})
	closeOnce := func() {
		once.Do(func() { close(stop) })
	}

	// The credential AND the scope the bridge was opened with, re-asked while
	// it is open: a session revoked or an account disabled after the upgrade
	// stops driving the run's Chromium, and so does a lost scope — a removed
	// membership, a demoted super-admin, a deleted run (runSocketStanding,
	// the run console's own per-envelope computation). r.Context() dies with
	// the upgrade, so the identity is snapshotted here. A bridge carrying no
	// credential is one no auth context ever reached (a server built without
	// the middleware) and has nothing to be asked about.
	if opener, ok := auth.FromContext(r.Context()); ok && opener.Credential != "" {
		go func() {
			defer closeOnce()
			tick := time.NewTicker(browserCDPStandingPeriod())
			defer tick.Stop()
			unreadable := 0
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					ctx, cancel := context.WithTimeout(context.Background(), browserCDPStandingTimeout)
					verdict := s.runSocketStanding(ctx, runID, opener)
					cancel()
					switch verdict.outcome {
					case socketStands:
						unreadable = 0
					case socketUnreadable:
						// The bridge is a command surface with no envelope
						// to refuse, so a standing nobody could re-check
						// keeps it only while the failures are isolated:
						// past the limit it closes fail-closed.
						unreadable++
						if unreadable < browserCDPStandingFailureLimit {
							continue
						}
						if s.logger != nil {
							s.logger.Warn("server: cdp bridge of run %s closed: %d consecutive standing reads failed", runID, unreadable)
						}
						_ = writeWSClose(conn, websocket.ClosePolicyViolation, wsCloseStandingUnavailable, "the standing of this connection could not be re-checked — reconnect")
						return
					case socketCredentialRefused:
						if s.logger != nil {
							s.logger.Warn("server: cdp bridge of run %s closed: %v", runID, verdict.cause)
						}
						_ = writeWSClose(conn, websocket.ClosePolicyViolation, wsCloseCredentialRefused, verdict.cause)
						return
					case socketScopeLost:
						if s.logger != nil {
							s.logger.Warn("server: cdp bridge of run %s closed: %v", runID, verdict.cause)
						}
						_ = writeWSClose(conn, websocket.ClosePolicyViolation, wsCloseScopeLost, verdict.cause)
						return
					}
				}
			}
		}()
	}

	// CDP → studio: re-frame the null-terminated pipe stream into
	// one WS BinaryMessage per CDP message. The pipe contract from
	// `--remote-debugging-pipe` is: one JSON-RPC object followed by
	// a single `\0` byte, both directions.
	//
	// scanOffset tracks how far we've already searched for `\0` in
	// the carry buffer, so a multi-MB CDP frame arriving in many
	// small reads doesn't degrade to O(N²) re-scans of the prefix.
	go func() {
		defer closeOnce()
		buf := make([]byte, 0, 32*1024)
		chunk := make([]byte, 32*1024)
		var scanOffset int
		for {
			n, readErr := sess.CDPConn.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
				for {
					rel := bytes.IndexByte(buf[scanOffset:], 0)
					if rel < 0 {
						scanOffset = len(buf)
						break
					}
					i := scanOffset + rel
					_ = conn.SetWriteDeadline(time.Now().Add(browserCDPWriteDeadline))
					if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:i]); writeErr != nil {
						return
					}
					// Drop the message + the `\0` separator and reset
					// the search head — the new prefix has not been
					// scanned yet.
					buf = buf[i+1:]
					scanOffset = 0
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					// The pipe error is the close reason, cut to what a
					// control frame carries (writeWSClose): an unbounded
					// reason makes the frame the client never receives, and
					// a dead pane reads as an abnormal transport drop
					// instead of the pipe's death.
					_ = writeWSClose(conn, websocket.CloseInternalServerErr, readErr.Error(), "")
				}
				return
			}
		}
	}()

	// Editor → CDP: each WS message is one CDP request; append `\0`
	// before forwarding to the pipe so Chromium's pipe parser can
	// find the boundary. Text frames are tolerated for clients that
	// don't pre-encode their JSON.
	go func() {
		defer closeOnce()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(browserCDPReadDeadline))
			mt, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
				continue
			}
			// Append the framing byte. The pipe contract guarantees
			// that one Write() with a complete message + `\0` is
			// atomic from the kernel's perspective for buffer sizes
			// well under PIPE_BUF (4096 on Linux), which CDP messages
			// almost always are. For very large outgoing messages
			// the kernel may split — Chromium handles that fine.
			if _, writeErr := sess.CDPConn.Write(append(payload, 0)); writeErr != nil {
				return
			}
		}
	}()

	<-stop
}

// handleBrowserAttach spawns a host Chromium via HostChromiumRunner
// and registers it as a BrowserSession on the given run, then emits
// the corresponding EventBrowserSessionStarted so the studio's
// reducer flips the Browser pane to live mode. Useful for testing
// the live pipeline end-to-end without wiring Playwright MCP
// detection at the manager level (a separate, larger workstream).
//
// POST /api/runs/{id}/browser/attach
//
// Origin-gated like the other mutating endpoints. Returns the
// session id so the caller can correlate or detach later.
func (s *Server) handleBrowserAttach(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "run id required")
		return
	}
	if s.runs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run console disabled")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), runID); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if s.browserSessions == nil {
		s.httpErrorFor(w, r, http.StatusServiceUnavailable, "browser pane disabled")
		return
	}

	// Generate a session id. 8 random bytes hex-encoded keeps it
	// short for the URL bar but uniquely-named across concurrent
	// attaches.
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "session id: %v", err)
		return
	}
	sessionID := hex.EncodeToString(idBytes[:])

	runner := mcp.NewHostChromiumRunner()
	conn, err := runner.Start(runID, "")
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "chromium: %v", err)
		return
	}

	if err := s.browserSessions.Attach(mcp.BrowserSession{
		SessionID: sessionID,
		RunID:     runID,
		CDPConn:   conn,
	}); err != nil {
		_ = conn.Close()
		s.httpErrorFor(w, r, http.StatusInternalServerError, "attach: %v", err)
		return
	}

	// The studio's BrowserPane handles state-update locally on the
	// 200 response (sets liveSession in the zustand store). We
	// intentionally do NOT persist a `browser_session_started`
	// event here: this debug-attach is a developer tool, not part
	// of the run's authoritative timeline. The Playwright MCP
	// auto-wire path emits the event from inside the runtime where
	// it has store access alongside the rest of the run's events.

	s.reflectAllowedOrigin(w, r)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"session_id": sessionID})
}

// requireBrowserRegistry is the small dependency-injection point used
// by tests + the desktop / cloud wiring. Both pass an mcp.BrowserRegistry
// at server construction time.
var _ = mcp.BrowserRegistry(nil)
