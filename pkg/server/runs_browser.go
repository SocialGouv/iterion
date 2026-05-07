package server

import (
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

	// CDP → editor: read raw bytes, push as BinaryMessage.
	go func() {
		defer closeOnce()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := sess.CDPConn.Read(buf)
			if n > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(browserCDPWriteDeadline))
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					// Send a final close frame with an error reason
					// so the client surfaces a meaningful disconnect
					// rather than a generic "ws closed".
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

	// Editor → CDP: read BinaryMessage frames, forward to the pipe.
	// Text frames from the editor are tolerated (the cdpClient may
	// send JSON commands as Text), but converted to bytes verbatim.
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
			if _, writeErr := sess.CDPConn.Write(payload); writeErr != nil {
				return
			}
		}
	}()

	<-stop
}

// requireBrowserRegistry is the small dependency-injection point used
// by tests + the desktop / cloud wiring. Both pass an mcp.BrowserRegistry
// at server construction time.
var _ = mcp.BrowserRegistry(nil)
