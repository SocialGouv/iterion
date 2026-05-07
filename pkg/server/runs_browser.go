package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
)

// browserCDPReadDeadline is the per-frame read budget on the editor →
// CDP direction. Editor inputs (mouse/keyboard) are tiny and bursty;
// a generous 60s window keeps a quiet idle pane from being torn down.
const browserCDPReadDeadline = 60 * time.Second

// browserCDPWriteDeadline bounds how long a single frame can sit in
// the OS send buffer before we declare the client gone. Screencast
// frames at 10fps × ~100KB push back-pressure quickly when the
// receiver is paused, so this cannot be too long.
const browserCDPWriteDeadline = 5 * time.Second

// browserCDPMaxFrame caps the inbound size from the editor — input
// events should never exceed a few hundred bytes, so 64 KiB is more
// than generous and protects against accidental flooding.
const browserCDPMaxFrame = 64 * 1024

// handleBrowserCDP serves GET /api/runs/{id}/browser/cdp?session=<id>.
// It bridges the editor's WebSocket to the in-process Chromium CDP
// pipe registered in the BrowserRegistry. CDP frames are JSON-RPC
// strings, but the proxy is a dumb binary pump — we never parse the
// payload, so a future wire-format upgrade upstream is invisible
// here.
//
// Auth: same Origin allowlist as the run console WS. Future PRs will
// add a per-run capability token so the editor can dial without
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
	if _, err := s.runs.LoadRun(runID); err != nil {
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
	conn.SetReadLimit(browserCDPMaxFrame)

	// Both pumps share an err channel; the first one to drain
	// shuts the other down.
	var once sync.Once
	stop := make(chan struct{})
	closeOnce := func() {
		once.Do(func() { close(stop) })
	}

	// CDP → editor: re-frame the null-terminated pipe stream into
	// one WS BinaryMessage per CDP message. The pipe contract from
	// `--remote-debugging-pipe` is: one JSON-RPC object followed by
	// a single `\0` byte, both directions.
	go func() {
		defer closeOnce()
		buf := make([]byte, 0, 32*1024)
		chunk := make([]byte, 32*1024)
		for {
			n, readErr := sess.CDPConn.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
				for {
					i := indexByteZero(buf)
					if i < 0 {
						break
					}
					message := buf[:i]
					_ = conn.SetWriteDeadline(time.Now().Add(browserCDPWriteDeadline))
					if writeErr := conn.WriteMessage(websocket.BinaryMessage, message); writeErr != nil {
						return
					}
					// Drop the message + the `\0` separator from the buffer.
					buf = buf[i+1:]
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					_ = conn.WriteMessage(
						websocket.CloseMessage,
						websocket.FormatCloseMessage(
							websocket.CloseInternalServerErr,
							readErr.Error(),
						),
					)
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

// indexByteZero returns the index of the first `\0` in buf, or -1.
// Mirrors bytes.IndexByte but kept inline so the hot path doesn't
// pay an extra package import.
func indexByteZero(buf []byte) int {
	for i, b := range buf {
		if b == 0 {
			return i
		}
	}
	return -1
}

// handleBrowserAttach spawns a host Chromium via HostChromiumRunner
// and registers it as a BrowserSession on the given run, then emits
// the corresponding EventBrowserSessionStarted so the editor's
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
	runID := r.PathValue("id")
	if runID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "run id required")
		return
	}
	if s.runs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run console disabled")
		return
	}
	if _, err := s.runs.LoadRun(runID); err != nil {
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

	// The editor's BrowserPane handles state-update locally on the
	// 200 response (sets liveSession in the zustand store). We
	// intentionally do NOT persist a `browser_session_started`
	// event here: this debug-attach is a developer tool, not part
	// of the run's authoritative timeline. PR 5 (Playwright MCP
	// auto-wire) will emit the event from inside the runtime where
	// it has store access alongside the rest of the run's events.

	s.reflectAllowedOrigin(w, r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"session_id": sessionID})
}

// requireBrowserRegistry is the small dependency-injection point used
// by tests + the desktop / cloud wiring. Both pass an mcp.BrowserRegistry
// at server construction time.
var _ = mcp.BrowserRegistry(nil)
