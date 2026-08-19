package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512
	sendBufferSize = 256
)

// FileEvent is the JSON message sent to WebSocket clients when a file changes.
type FileEvent struct {
	Type string `json:"type"` // file_created, file_modified, file_deleted
	Path string `json:"path"` // relative to WorkDir
}

// Hub maintains the set of active WebSocket clients and broadcasts messages to them.
type Hub struct {
	logger     *iterlog.Logger
	clients    map[*wsClient]bool
	broadcast  chan []byte
	register   chan *wsClient
	unregister chan *wsClient
	done       chan struct{}
	stopOnce   sync.Once // guards done against a double close (panic)
}

// wsClient wraps a single WebSocket connection.
type wsClient struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// upgrader for the studio file-event WebSocket. CheckOrigin enforces a
// loopback origin allowlist injected by the Hub at construction time:
// without this, any browser tab could open ws://localhost:<port>/api/ws and
// observe every file change in WorkDir. The previous "return true" was
// justified as 'acceptable for a local-only studio server' but the threat
// here is browser-side / drive-by, which a local-only bind does not stop.
//
// Empty-Origin policy: browsers always set Origin on cross-origin WS
// handshakes, so a missing Origin means a non-browser local caller
// (curl, websocat) — which on a shared host (CI runner, multi-user
// devcontainer) is a different trust boundary than the operator's own
// browser. /api/ws/runs/{id} accepts state-changing cmds (cancel,
// answer, resume) post-upgrade with parity to HTTP cancel/resume; HTTP
// counterparts gate via requireSafeOrigin which rejects unauthorised
// origins but lets through empty-Origin (curl). The WS upgrader keeps
// that parity by default but operators in hostile environments can
// tighten it via ITERION_REQUIRE_WS_ORIGIN=1, which refuses any
// upgrade without a valid Origin header.
var upgrader = websocket.Upgrader{
	// Per-message deflate (RFC 7692). Event payloads are very
	// JSON-ish — repetitive keys, monotonic seqs, similar event types
	// page-to-page — so the compression ratio is typically 5–10× for
	// the replay path and ~3× for live events. On localhost the win
	// is mostly CPU (less time spent JSON-parsing on the frontend);
	// on remote dev/cloud the bandwidth saving compounds.
	EnableCompression: true,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			if os.Getenv("ITERION_REQUIRE_WS_ORIGIN") == "1" {
				return false
			}
			// Non-browser caller (curl, websocat without --origin) — allow.
			// Browsers always send Origin on cross-origin WS handshakes.
			return true
		}
		// Same-origin — the SPA dialing the host that served it (local, behind
		// a proxy, or in the cloud) — is always allowed: a cross-site drive-by
		// carries a different Origin host and falls through to the allowlist
		// below. Deployment-agnostic, so the served studio works on any host
		// without configuring its public URL.
		if sameOrigin(origin, r) {
			return true
		}
		check := loadOriginCheck()
		if check == nil {
			// Defensive fallback: refuse rather than re-open the hole if
			// the Hub forgot to register an origin checker.
			return false
		}
		return check(origin)
	},
}

// sameOrigin reports whether the request's Origin header host matches the
// request's own Host (the SPA dialing the host that served it). Browsers set
// Origin to the SPA's true origin; a request from another site carries a
// different host and is rejected by the allowlist instead. Shared by the WS
// upgrader (CheckOrigin) and the HTTP CORS path (isAllowedOriginReq) so both
// transports treat the deployed/cloud studio identically.
func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// currentOriginCheck is set by Server.routes() to the same allowlist used
// by the HTTP CORS path. Keeping it as a package-level var avoids changing
// gorilla/websocket's CheckOrigin signature, which can't carry a closure
// per-Hub instance without forking the upgrader struct per Hub.
//
// Stored via atomic.Pointer so parallel server.New() calls (notably the
// `go test -race` test setup) don't race writes against the read path
// inside upgrader.CheckOrigin. The function is replaced wholesale; no
// CAS required.
var currentOriginCheck atomic.Pointer[originCheckFn]

type originCheckFn func(origin string) bool

func loadOriginCheck() originCheckFn {
	if p := currentOriginCheck.Load(); p != nil {
		return *p
	}
	return nil
}

// SetWebSocketOriginCheck registers the origin allowlist function used by
// the WebSocket upgrader. Called by Server during routes() setup.
func SetWebSocketOriginCheck(fn func(origin string) bool) {
	if fn == nil {
		currentOriginCheck.Store(nil)
		return
	}
	wrapped := originCheckFn(fn)
	currentOriginCheck.Store(&wrapped)
}

// NewHub creates a new Hub.
func NewHub(logger *iterlog.Logger) *Hub {
	return &Hub{
		logger:     logger,
		clients:    make(map[*wsClient]bool),
		broadcast:  make(chan []byte, 64),
		register:   make(chan *wsClient),
		unregister: make(chan *wsClient),
		done:       make(chan struct{}),
	}
}

// Run processes register, unregister, and broadcast events. Call as a goroutine.
func (h *Hub) Run() {
	for {
		select {
		case <-h.done:
			for c := range h.clients {
				close(c.send)
				delete(h.clients, c)
			}
			return
		case c := <-h.register:
			h.clients[c] = true
		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}
		case msg := <-h.broadcast:
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					close(c.send)
					delete(h.clients, c)
				}
			}
		}
	}
}

// Stop signals the hub to shut down. Idempotent: closing done twice would
// panic, so the close is guarded by sync.Once (matching the other Stop/Close
// methods in this tree, e.g. dispatcher's config_watcher / engine_runner).
func (h *Hub) Stop() {
	h.stopOnce.Do(func() { close(h.done) })
}

// Broadcast marshals a FileEvent to JSON and sends it to all connected clients.
func (h *Hub) Broadcast(event FileEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		h.logger.Error("hub: marshal error: %v", err)
		return
	}
	h.broadcastBytes(data)
}

// broadcastBytes pushes an already-JSON-encoded payload through the
// same client fan-out as Broadcast. Used by sibling files in this
// package (projects.go) to emit non-FileEvent payloads on the same
// WebSocket channel; the marshaling and typing stay at the call site
// so the Hub itself stays agnostic about payload shape.
func (h *Hub) broadcastBytes(data []byte) {
	select {
	case h.broadcast <- data:
	case <-h.done:
	}
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and registers the client.
//
// Shutdown safety: if the hub has already been Stop()'d (h.done closed), the
// Run() goroutine has exited and `h.register` has no receiver — a naive
// `h.register <- c` here would block the HTTP handler goroutine forever and
// leak its conn. We short-circuit on that and refuse the upgrade. We also
// guard the register send itself with a select-on-done in case Stop fires
// concurrently with this handler.
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	select {
	case <-h.done:
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	default:
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("ws upgrade error: %v", err)
		return
	}
	c := &wsClient{hub: h, conn: conn, send: make(chan []byte, sendBufferSize)}
	select {
	case h.register <- c:
	case <-h.done:
		// Hub stopped between the entry check and now — close the conn
		// and bail rather than leaking a goroutine on the unbuffered
		// register channel.
		_ = conn.Close()
		return
	}
	errtrack.Go("server.hub.writePump", c.writePump)
	errtrack.Go("server.hub.readPump", c.readPump)
}

// readPump reads and discards incoming messages, detecting disconnection.
//
// On exit, the client must be unregistered from the hub. If the hub has
// already shut down (h.done closed), Run() has stopped consuming from
// h.unregister, and a naive send would block this goroutine forever.
// Guard the send with a select-on-done so shutdown unblocks it.
func (c *wsClient) readPump() {
	defer func() {
		select {
		case c.hub.unregister <- c:
		case <-c.hub.done:
		}
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	// Keepalive read deadlines are best-effort: on a dead conn the read loop
	// below breaks at the next ReadMessage error anyway.
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			break
		}
	}
}

// writePump drains the send channel to the WebSocket connection.
func (c *wsClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
