package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	iterlog "github.com/SocialGouv/iterion/log"
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
}

// wsClient wraps a single WebSocket connection.
type wsClient struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// upgrader accepts all origins — acceptable for a local-only editor server.
// Restrict CheckOrigin if the server is ever exposed on a network.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
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

// Stop signals the hub to shut down.
func (h *Hub) Stop() {
	close(h.done)
}

// Broadcast marshals a FileEvent to JSON and sends it to all connected clients.
func (h *Hub) Broadcast(event FileEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		h.logger.Error("hub: marshal error: %v", err)
		return
	}
	select {
	case h.broadcast <- data:
	case <-h.done:
	}
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and registers the client.
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("ws upgrade error: %v", err)
		return
	}
	c := &wsClient{hub: h, conn: conn, send: make(chan []byte, sendBufferSize)}
	h.register <- c
	go c.writePump()
	go c.readPump()
}

// readPump reads and discards incoming messages, detecting disconnection.
func (c *wsClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
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
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
