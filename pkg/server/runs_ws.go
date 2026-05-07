package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// runWSEnvelope is the wire shape for every WS message in either direction.
// Type discriminates the payload; AckID is optional and echoed by the
// server on responses to client→server commands so the client can match
// replies to in-flight requests.
type runWSEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	AckID   string          `json:"ack_id,omitempty"`
}

// Server→client message types.
const (
	wsTypeSnapshot   = "snapshot"
	wsTypeEvent      = "event"
	wsTypeError      = "error"
	wsTypeAck        = "ack"
	wsTypeTerminated = "terminated"
	wsTypeLogChunk   = "log_chunk"
	// wsTypeLogTerminated signals end of the log stream for a run.
	// Distinct from wsTypeTerminated which signals end of the event
	// stream — a UI can keep its log panel rendered with the final
	// content while the events panel transitions to "completed".
	wsTypeLogTerminated = "log_terminated"
)

// Client→server message types.
const (
	wsTypeSubscribe      = "subscribe"
	wsTypeUnsubscribe    = "unsubscribe"
	wsTypeCancel         = "cancel"
	wsTypeAnswer         = "answer"
	wsTypeSubscribeLogs  = "subscribe_logs"
	wsTypeUnsubscribeLog = "unsubscribe_logs"
)

type wsSubscribeRequest struct {
	FromSeq int64 `json:"from_seq,omitempty"`
}

type wsSubscribeLogsRequest struct {
	FromOffset int64 `json:"from_offset,omitempty"`
}

type wsLogChunkPayload struct {
	Offset int64  `json:"offset"`
	Text   string `json:"text"`
	// Total is the buffer's running write counter at the moment this
	// chunk was emitted. Lets the client detect drops (offset gap)
	// and decide to re-anchor via /api/runs/{id}/log.
	Total int64 `json:"total,omitempty"`
}

type wsAnswerRequest struct {
	FilePath string                 `json:"file_path,omitempty"` // optional; falls back to run.FilePath
	Source   string                 `json:"source,omitempty"`    // see resumeRunRequest.Source
	Answers  map[string]interface{} `json:"answers"`
}

type wsErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleRunWebSocket upgrades a connection to /api/ws/runs/{id} and runs
// the read+write pumps for one subscriber. The pump pair is single-
// connection state: each client gets its own goroutine pair. The Hub
// abstraction used by the file-watcher endpoint isn't reused here
// because per-run subscriptions are inherently single-recipient and
// state-bound, while the Hub broadcasts one stream to N clients.
func (s *Server) handleRunWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.runs == nil {
		http.Error(w, "run console not configured", http.StatusServiceUnavailable)
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("ws upgrade error: %v", err)
		return
	}
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.WSConnections.Inc()
	}
	rc := newRunConn(s, conn, runID)
	go rc.run()
}

// runConn owns one WS subscription. The read pump parses inbound
// commands and forwards them to handler methods; the write pump
// serialises outgoing envelopes. A single sendCh between them keeps
// writes single-threaded so the gorilla connection never sees
// concurrent writes (which would corrupt frames).
type runConn struct {
	server *Server
	conn   *websocket.Conn
	runID  string
	sendCh chan []byte

	mu            sync.Mutex
	subscribed    bool
	sub           *runview.EventSubscription
	logSubscribed bool
	logSub        *runview.RunLogSubscription
	closeOnce     sync.Once
	closed        chan struct{}
}

func newRunConn(s *Server, conn *websocket.Conn, runID string) *runConn {
	return &runConn{
		server: s,
		conn:   conn,
		runID:  runID,
		sendCh: make(chan []byte, 256),
		closed: make(chan struct{}),
	}
}

func (c *runConn) run() {
	defer c.close()
	go c.writePump()
	c.readPump()
}

func (c *runConn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.mu.Lock()
		if c.sub != nil {
			c.sub.Cancel()
			c.sub = nil
		}
		if c.logSub != nil {
			c.logSub.Cancel()
			c.logSub = nil
		}
		c.mu.Unlock()
		_ = c.conn.Close()
		if c.server.cfg.Metrics != nil {
			c.server.cfg.Metrics.WSConnections.Dec()
		}
	})
}

// readPump parses inbound envelopes and dispatches each command. Any
// parse / handler error is sent back as an `error` envelope and the
// connection is kept open — a single bad message shouldn't tear down
// the live event stream.
func (c *runConn) readPump() {
	c.conn.SetReadLimit(1 << 20) // 1 MB — answers can be substantial
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var env runWSEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.sendError("bad_envelope", err.Error(), "")
			continue
		}
		c.dispatch(env)
	}
}

func (c *runConn) dispatch(env runWSEnvelope) {
	switch env.Type {
	case wsTypeSubscribe:
		c.handleSubscribe(env)
	case wsTypeUnsubscribe:
		c.handleUnsubscribe(env)
	case wsTypeSubscribeLogs:
		c.handleSubscribeLogs(env)
	case wsTypeUnsubscribeLog:
		c.handleUnsubscribeLogs(env)
	case wsTypeCancel:
		c.handleCancel(env)
	case wsTypeAnswer:
		c.handleAnswer(env)
	default:
		c.sendError("unknown_type", "unknown message type: "+env.Type, env.AckID)
	}
}

// handleSubscribe registers the connection with the broker and sends
// the catch-up sequence: snapshot first, then any persisted events
// with seq >= from_seq, then the live tail. Calling subscribe twice
// on the same connection is a no-op (acked but nothing changes); use
// unsubscribe + subscribe to re-anchor at a different from_seq.
func (c *runConn) handleSubscribe(env runWSEnvelope) {
	var req wsSubscribeRequest
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			c.sendError("bad_payload", err.Error(), env.AckID)
			return
		}
	}

	c.mu.Lock()
	if c.subscribed {
		c.mu.Unlock()
		c.sendAck(env.AckID)
		return
	}
	// In cloud mode (Mongo change-stream source wired) we skip the
	// in-process broker entirely — the Mongo source handles both
	// historical replay and live tail in a single Subscribe call,
	// and the broker only sees this process's writes anyway. In
	// local mode we keep the broker so multi-WS clients on the same
	// process share the same fan-out cursor.
	if !c.server.runs.HasEventSource() {
		c.sub = c.server.runs.Broker().Subscribe(c.runID)
	}
	c.subscribed = true
	c.mu.Unlock()

	snap, err := c.server.runs.Snapshot(c.runID)
	if err != nil {
		c.sendError("snapshot_failed", err.Error(), env.AckID)
		return
	}
	c.sendEnvelope(wsTypeSnapshot, snap, env.AckID)

	go c.streamEvents(req.FromSeq, snap.LastSeq)
}

// streamEvents replays historical events then tails the live source.
// Two implementations behind the same WS contract:
//
//   - local broker mode: replay via store.LoadEvents(fromSeq, snap+1),
//     then drain the in-process EventBroker channel until the run
//     terminates (channel closes).
//   - cloud event-source mode: a single eventstream.Source subscription
//     handles both phases — the source emits historical first, then
//     transitions to a Mongo change-stream tail, with no boundary
//     dedup needed (the source itself avoids the gap).
//
// Plan §F (T-21).
func (c *runConn) streamEvents(fromSeq, snapshotSeq int64) {
	if c.server.runs.HasEventSource() {
		c.streamEventsCloud(fromSeq)
		return
	}
	c.streamEventsLocal(fromSeq, snapshotSeq)
}

// streamEventsLocal is the original broker-backed path: replay disk
// events [fromSeq, snapshotSeq+1) then drain the broker channel,
// dedup'ing against the replay window.
func (c *runConn) streamEventsLocal(fromSeq, snapshotSeq int64) {
	if snapshotSeq != runview.NoEventsSeq && (fromSeq > 0 || snapshotSeq > 0) {
		events, err := c.server.runs.LoadEvents(c.runID, fromSeq, snapshotSeq+1)
		if err == nil {
			for _, ev := range events {
				if !c.sendEnvelope(wsTypeEvent, ev, "") {
					return
				}
			}
		}
	}

	c.mu.Lock()
	sub := c.sub
	c.mu.Unlock()
	if sub == nil {
		return
	}

	for {
		select {
		case <-c.closed:
			return
		case ev, ok := <-sub.C:
			if !ok {
				c.sendEnvelope(wsTypeTerminated, map[string]string{"run_id": c.runID}, "")
				return
			}
			if snapshotSeq != runview.NoEventsSeq && ev.Seq <= snapshotSeq {
				continue
			}
			if !c.sendEnvelope(wsTypeEvent, ev, "") {
				return
			}
		}
	}
}

// streamEventsCloud subscribes to the Mongo change-stream source. The
// source emits historical replay events (seq >= fromSeq) followed by
// live ones from the change stream; no dedup or boundary tracking is
// needed on this side — the source guarantees no gaps and no
// duplicates. The subscription's Errors channel is drained but not
// fatal: a transient change-stream blip is logged and the WS stays
// open until c.closed fires. The source's underlying ctx is bound to
// c.closed via Close() in the defer, so we don't need a separate
// goroutine to translate the channel into a cancel.
func (c *runConn) streamEventsCloud(fromSeq int64) {
	sub, err := c.server.runs.SubscribeEventStream(context.Background(), c.runID, fromSeq)
	if err != nil {
		c.sendError("event_stream_failed", err.Error(), "")
		return
	}
	defer func() { _ = sub.Close() }()

	events := sub.Events()
	errs := sub.Errors()
	for {
		select {
		case <-c.closed:
			return
		case ev, ok := <-events:
			if !ok {
				c.sendEnvelope(wsTypeTerminated, map[string]string{"run_id": c.runID}, "")
				return
			}
			if !c.sendEnvelope(wsTypeEvent, ev, "") {
				return
			}
		case err, ok := <-errs:
			if !ok {
				continue
			}
			if c.server.logger != nil {
				c.server.logger.Warn("server: ws event stream %s: %v", c.runID, err)
			}
		}
	}
}

func (c *runConn) handleUnsubscribe(env runWSEnvelope) {
	c.mu.Lock()
	if c.sub != nil {
		c.sub.Cancel()
		c.sub = nil
	}
	c.subscribed = false
	c.mu.Unlock()
	c.sendAck(env.AckID)
}

func (c *runConn) handleCancel(env runWSEnvelope) {
	// Source-attribute the cancel: pairs with the HTTP cancel log line
	// in runs.go so a "context canceled" mid-run failure can be traced
	// back to either an explicit user click (HTTP endpoint) or a WS
	// envelope from a connected client.
	if c.server.logger != nil {
		c.server.logger.Info("server: cancel run %q via WS from %s", c.runID, c.conn.RemoteAddr())
	}
	if err := c.server.runs.Cancel(c.runID); err != nil && !errors.Is(err, runview.ErrRunNotActive) {
		c.sendError("cancel_failed", err.Error(), env.AckID)
		return
	}
	c.sendAck(env.AckID)
}

func (c *runConn) handleAnswer(env runWSEnvelope) {
	var req wsAnswerRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		c.sendError("bad_payload", err.Error(), env.AckID)
		return
	}
	if len(req.Answers) == 0 {
		c.sendError("no_answers", "answers is required", env.AckID)
		return
	}
	filePath := req.FilePath
	if filePath == "" {
		runMeta, err := c.server.runs.LoadRun(c.runID)
		if err != nil {
			c.sendError("run_not_found", err.Error(), env.AckID)
			return
		}
		filePath = runMeta.FilePath
		if filePath == "" && req.Source == "" {
			c.sendError("file_path_required", "run has no persisted FilePath; supply file_path or source in payload", env.AckID)
			return
		}
	}
	absPath, err := c.server.resolveWorkflowPath(filePath, req.Source)
	if err != nil {
		c.sendError("invalid_file_path", err.Error(), env.AckID)
		return
	}
	// Detach from WS-connection ctx: closing the browser tab must not
	// cancel an in-flight resume. The service's manager owns lifecycle.
	if _, err := c.server.runs.Resume(context.Background(), runview.ResumeSpec{
		RunID:    c.runID,
		FilePath: absPath,
		Source:   req.Source,
		Answers:  req.Answers,
	}); err != nil {
		c.sendError("resume_failed", err.Error(), env.AckID)
		return
	}
	c.sendAck(env.AckID)
}

// sendEnvelope marshals and queues a server→client envelope. Returns
// false if the connection is being torn down.
func (c *runConn) sendEnvelope(t string, payload interface{}, ackID string) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		c.server.logger.Error("ws marshal payload: %v", err)
		return true
	}
	env := runWSEnvelope{Type: t, Payload: body, AckID: ackID}
	data, err := json.Marshal(env)
	if err != nil {
		c.server.logger.Error("ws marshal envelope: %v", err)
		return true
	}
	select {
	case c.sendCh <- data:
		return true
	case <-c.closed:
		return false
	}
}

func (c *runConn) sendError(code, msg, ackID string) {
	c.sendEnvelope(wsTypeError, wsErrorPayload{Code: code, Message: msg}, ackID)
}

func (c *runConn) sendAck(ackID string) {
	if ackID == "" {
		return
	}
	c.sendEnvelope(wsTypeAck, map[string]string{}, ackID)
}

// writePump drains sendCh to the WebSocket connection and emits
// periodic ping frames so idle connections don't time out at NAT/LB
// hops.
func (c *runConn) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case data, ok := <-c.sendCh:
			if !ok {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.close()
				return
			}
		}
	}
}
