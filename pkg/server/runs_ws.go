package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
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
	wsTypeSnapshot = "snapshot"
	wsTypeEvent    = "event"
	// wsTypeEventBatch is the bulk equivalent of wsTypeEvent: payload is
	// an array of events instead of one. Used for historical replay so
	// the server marshals one envelope per page (up to MaxEventsPerPage)
	// instead of one per event, and the frontend dispatches one state
	// update per page instead of one per event. Live (broker-driven)
	// events keep using wsTypeEvent — they arrive one at a time and
	// batching them would just add latency without saving any frames.
	wsTypeEventBatch = "event_batch"
	wsTypeError      = "error"
	wsTypeAck        = "ack"
	wsTypeTerminated = "terminated"
	wsTypeLogChunk   = "log_chunk"
	// wsTypeLogTerminated signals end of the log stream for a run.
	// Distinct from wsTypeTerminated which signals end of the event
	// stream — a UI can keep its log panel rendered with the final
	// content while the events panel transitions to "completed".
	wsTypeLogTerminated = "log_terminated"
	// wsTypePong answers a client wsTypePing. It is the JS-observable
	// liveness proof the browser's automatic control-frame pong is NOT:
	// the WebSocket API never surfaces server ping frames to onmessage,
	// so an idle-but-alive run produces zero client-visible traffic and
	// a half-open socket (peer vanished without FIN) is never noticed.
	// The client's application-level heartbeat pings and watches for
	// this pong to detect a dead connection and force a reconnect.
	wsTypePong = "pong"
)

// Client→server message types.
const (
	wsTypeSubscribe      = "subscribe"
	wsTypeUnsubscribe    = "unsubscribe"
	wsTypeCancel         = "cancel"
	wsTypePause          = "pause"
	wsTypeAnswer         = "answer"
	wsTypeSubscribeLogs  = "subscribe_logs"
	wsTypeUnsubscribeLog = "unsubscribe_logs"
	// wsTypeQueueMessage queues an operator chat message against a
	// running agent. Payload is wsQueueMessageRequest. Reply is an
	// ack envelope with the QueuedUserMessage record as payload.
	wsTypeQueueMessage = "queue_message"
	// wsTypeCancelQueuedMessage cancels a message that has not yet
	// been delivered. Payload is wsCancelQueuedMessageRequest.
	wsTypeCancelQueuedMessage = "cancel_queued_message"
	// wsTypePing is the client's application-level heartbeat. The server
	// answers with wsTypePong (see wsTypePong for why the WS control-frame
	// ping/pong is insufficient for client-side liveness detection).
	wsTypePing = "ping"
	// wsTypeBumpLoop / wsTypeRaiseBudget are the live-steering commands
	// (grant loop iterations / raise budget caps on a RUNNING run).
	// Payloads are wsBumpLoopRequest / wsRaiseBudgetRequest; the reply
	// is an ack envelope carrying the runview response struct. Human
	// answers already ride wsTypeAnswer.
	wsTypeBumpLoop    = "bump_loop"
	wsTypeRaiseBudget = "raise_budget"
)

type wsSubscribeRequest struct {
	FromSeq int64 `json:"from_seq,omitempty"`
	// ReplayHistory tells the server whether to send disk-persisted
	// events in the catch-up phase between snapshot and live tail.
	// Default (false) means "lazy": the client gets the snapshot and
	// the live tail, but no historical replay — saving the cost of
	// streaming thousands of events the studio doesn't need to render
	// the canvas or status pill. Consumers that DO need history
	// (EventLog tab, Scrubber) fetch it via GET /api/runs/{id}/events
	// when they mount. Set explicitly to true on WS reconnect after a
	// transient disconnect so the gap between FromSeq and snapshotSeq
	// is recovered.
	ReplayHistory bool `json:"replay_history,omitempty"`
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
	FilePath string         `json:"file_path,omitempty"` // optional; falls back to run.FilePath
	Source   string         `json:"source,omitempty"`    // see resumeRunRequest.Source
	Answers  map[string]any `json:"answers"`
}

type wsQueueMessageRequest struct {
	Text   string   `json:"text"`
	Skills []string `json:"skills,omitempty"`
}

type wsCancelQueuedMessageRequest struct {
	MessageID string `json:"message_id"`
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
//
// Cross-store mode: when `?store=<path>` is present (and valid under
// $HOME/.iterion/**), the subscription reads snapshots + tails events
// from THAT store instead of the daemon's primary. State-changing
// commands (cancel, resume, answer) are rejected with cross_store_readonly
// in this mode since we don't drive the foreign run's engine — its
// owning daemon (or CLI process) does.
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
	// Sanitize before the run ID is path-joined into log/event files
	// downstream (cross-store run.log / events.jsonl tail, log replay).
	// The store's own readers go through this guard; the direct file
	// readers reached from this WS handler must too.
	if err := store.SanitizePathComponent("run ID", runID); err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	// Cross-store check BEFORE upgrade so an invalid store= produces a
	// clean HTTP 400 instead of a WS error envelope at first message.
	xStore, xStorePath, err := s.resolveCrossStore(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Authorize access to runID BEFORE upgrading. requireAuth has
	// already validated the bearer token (via authMiddleware) and
	// stamped the tenant identity on r.Context(). For the primary-
	// store path we now confirm the run is visible to that identity
	// — mongo's LoadRun applies the tenant filter, so a cross-team
	// or unknown ID yields not-found. Returning HTTP 404/403 here
	// instead of completing the upgrade and replying with a WS
	// error envelope makes the wire behavior unambiguous and stops
	// us from accounting WSConnections for forbidden subscriptions.
	// Cross-store mode skips this — the foreign FS store has no
	// tenant scoping and the resolveCrossStore() above already
	// gated the path under $HOME/.iterion/**.
	if xStore == nil {
		if _, lerr := s.runs.LoadRunCtx(r.Context(), runID); lerr != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
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
	rc.xStore = xStore
	rc.xStorePath = xStorePath
	// One store-agnostic streaming source per connection (ADR-053): a
	// per-conn FileSource for cross-store observation (closed with the
	// conn — see close()), the Service's shared source otherwise.
	if xStore != nil {
		rc.src = runstream.NewFileSource(xStore, xStorePath, s.logger)
	} else {
		rc.src = s.runs.StreamSource()
	}
	// Snapshot the authenticated tenant identity so every per-WS store
	// call (Snapshot, LoadEvents, CancelInactive, LoadRun) carries the
	// same tenant_id that requireAuth stamped on the upgrade request.
	// r.Context() itself can't be reused after Upgrade returns, but
	// the tenant/user identity it carried is what mongo's filter keys
	// on; stamping them onto a fresh background ctx preserves
	// isolation across the WS lifetime.
	if tenantID, ok := store.TenantFromContext(r.Context()); ok {
		userID, _ := store.OwnerFromContext(r.Context())
		rc.tenantID = tenantID
		rc.userID = userID
	}
	// Snapshot the RBAC identity (TeamID + IsSuperAdmin) too — the store
	// tenant tag above does not carry it, and the suspend gate on
	// answer→resume (handleAnswer) needs it to evaluate org status.
	if id, ok := auth.FromContext(r.Context()); ok {
		rc.identity = id
	}
	errtrack.Go("server.runWS", rc.run)
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

	// xStore is set when the WS connected with `?store=<path>` query —
	// the snapshot + event-tail come from this foreign store instead
	// of the daemon's primary. Read-only: state-changing commands
	// (cancel/resume/answer) are rejected in this mode.
	// xStorePath is the resolved on-disk path of xStore (used to
	// locate <path>/runs/<runID>/events.jsonl for tailing).
	xStore     store.RunStore
	xStorePath string

	// tenantID / userID snapshot the auth identity at upgrade time
	// so per-WS store calls keep mongo's tenant_id filter applied.
	// Both empty in DisableAuth dev mode.
	tenantID string
	userID   string

	// identity snapshots the full RBAC identity (TeamID + IsSuperAdmin)
	// at upgrade time. The store tenant tag above does not carry it, so
	// suspend-gate checks on state-changing commands (answer→resume)
	// read this instead. Zero value in DisableAuth dev mode — but dev
	// mode stamps a super-admin identity, which the gate lets through.
	identity auth.Identity

	// src is the store-agnostic streaming source every subscription on
	// this connection goes through (ADR-053). Cross-store conns own a
	// per-connection FileSource, closed with the conn.
	src runstream.Source

	mu            sync.Mutex
	subscribed    bool
	eventSub      runstream.EventSubscription
	logSubscribed bool
	logSub        runstream.LogSubscription
	closeOnce     sync.Once
	closed        chan struct{}
}

// authCtx returns a fresh background ctx with the tenant/user
// identity captured at WS upgrade time stamped on it, so every per-
// WS store call applies the mongo tenant_id filter even though
// r.Context() from the upgrade isn't reusable.
func (c *runConn) authCtx() context.Context {
	if c.tenantID == "" {
		return context.Background()
	}
	return store.WithIdentity(context.Background(), c.tenantID, c.userID)
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
	errtrack.Go("server.runWS.writePump", c.writePump)
	c.readPump()
}

func (c *runConn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.mu.Lock()
		if c.eventSub != nil {
			_ = c.eventSub.Close()
			c.eventSub = nil
		}
		if c.logSub != nil {
			_ = c.logSub.Close()
			c.logSub = nil
		}
		c.mu.Unlock()
		if c.xStore != nil && c.src != nil {
			_ = c.src.Close() // per-connection cross-store FileSource
		}
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
	case wsTypePause:
		c.handlePause(env)
	case wsTypeAnswer:
		c.handleAnswer(env)
	case wsTypeQueueMessage:
		c.handleQueueMessage(env)
	case wsTypeCancelQueuedMessage:
		c.handleCancelQueuedMessage(env)
	case wsTypeBumpLoop:
		c.handleBumpLoop(env)
	case wsTypeRaiseBudget:
		c.handleRaiseBudget(env)
	case wsTypePing:
		// Application-level heartbeat: reply immediately so the client's
		// watchdog sees JS-observable inbound traffic and knows the
		// connection is alive. Stateless — echoes the ack_id if present.
		c.sendEnvelope(wsTypePong, map[string]string{}, env.AckID)
	default:
		c.sendError("unknown_type", "unknown message type: "+env.Type, env.AckID)
	}
}

// handleSubscribe registers the connection's event subscription and
// sends the catch-up sequence: snapshot first, then (with
// replay_history) any persisted events with seq >= from_seq, then the
// live tail — all delivered by the connection's store-agnostic source
// (ADR-053), whatever mode produced the run. Calling subscribe twice on
// the same connection is a no-op (acked but nothing changes); use
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
	c.subscribed = true
	c.mu.Unlock()

	snap, err := c.snapshot()
	if err != nil {
		c.mu.Lock()
		c.subscribed = false
		c.mu.Unlock()
		c.sendError("snapshot_failed", err.Error(), env.AckID)
		return
	}
	c.sendEnvelope(wsTypeSnapshot, snap, env.AckID)

	// Lazy mode: advance the effective replay floor past the snapshot
	// so the source sees "nothing to replay". Live tail still flows
	// unaffected. The frontend pulls history on demand via
	// /api/runs/{id}/events.
	effectiveFromSeq := req.FromSeq
	if !req.ReplayHistory && snap.LastSeq != runview.NoEventsSeq {
		effectiveFromSeq = snap.LastSeq + 1
	}

	sub, err := c.src.SubscribeEvents(c.authCtx(), c.runID, effectiveFromSeq)
	if err != nil {
		c.mu.Lock()
		c.subscribed = false
		c.mu.Unlock()
		c.sendError("event_stream_failed", err.Error(), "")
		return
	}
	c.mu.Lock()
	c.eventSub = sub
	c.mu.Unlock()
	errtrack.Go("server.runWS.pumpEvents", func() { c.pumpEvents(sub) })
}

// snapshot builds the subscribe-time snapshot from whichever store this
// connection observes — the only remaining storage difference in the
// WS layer (the foreign store has no Service wrapper to ask).
func (c *runConn) snapshot() (*runview.RunSnapshot, error) {
	if c.xStore != nil {
		return runview.BuildSnapshot(context.Background(), c.xStore, c.runID)
	}
	return c.server.runs.SnapshotCtx(c.authCtx(), c.runID)
}

// pumpEvents forwards the subscription to the WS: replay pages ship as
// event_batch envelopes (one frame per page instead of one per event),
// live deliveries as single event envelopes. The Events channel closing
// means the stream ended — a corrupted event log (surfaced on Errors
// right before the close) becomes an events_corrupted error envelope,
// any other end becomes terminated. Transient source errors (e.g. a
// change-stream reconnect notice) are logged and the stream stays open.
func (c *runConn) pumpEvents(sub runstream.EventSubscription) {
	defer func() { _ = sub.Close() }()
	events := sub.Events()
	errs := sub.Errors()
	var fatal error
	for {
		select {
		case <-c.closed:
			return
		case batch, ok := <-events:
			if !ok {
				if errors.Is(fatal, store.ErrEventsCorrupted) {
					c.sendError("events_corrupted", fatal.Error(), "")
				} else {
					c.sendEnvelope(wsTypeTerminated, map[string]string{"run_id": c.runID}, "")
				}
				return
			}
			if len(batch) == 1 {
				if !c.sendEnvelope(wsTypeEvent, batch[0], "") {
					return
				}
			} else if len(batch) > 0 {
				if !c.sendEnvelope(wsTypeEventBatch, batch, "") {
					return
				}
			}
		case err, ok := <-errs:
			if !ok {
				// Source closed Errors but may keep Events open. Nil the
				// channel so this case stops firing — a closed channel
				// is always ready to receive, which would spin a core.
				errs = nil
				continue
			}
			fatal = err
			if c.server.logger != nil {
				c.server.logger.Warn("server: ws event stream %s: %v", c.runID, err)
			}
		}
	}
}

func (c *runConn) handleUnsubscribe(env runWSEnvelope) {
	c.mu.Lock()
	if c.eventSub != nil {
		_ = c.eventSub.Close()
		c.eventSub = nil
	}
	c.subscribed = false
	c.mu.Unlock()
	c.sendAck(env.AckID)
}

func (c *runConn) handleCancel(env runWSEnvelope) {
	if c.xStore != nil {
		c.sendError("cross_store_readonly", "cancel is not available for cross-store runs — open the owning daemon to cancel", env.AckID)
		return
	}
	// Source-attribute the cancel: pairs with the HTTP cancel log line
	// in runs.go so a "context canceled" mid-run failure can be traced
	// back to either an explicit user click (HTTP endpoint) or a WS
	// envelope from a connected client.
	if c.server.logger != nil {
		c.server.logger.Info("server: cancel run %q via WS from %s", c.runID, c.conn.RemoteAddr())
	}
	err := c.server.runs.Cancel(c.runID)
	if err != nil && errors.Is(err, runview.ErrRunNotActive) {
		// Match the HTTP handler's behaviour: dispatcher-spawned runs
		// aren't tracked by the runview Manager — try the dispatcher's
		// own cancel-by-runID path first, then fall through to flipping
		// a paused / failed_resumable run to cancelled.
		if c.server.cfg.Dispatcher != nil && c.server.cfg.Dispatcher.CancelRun(c.runID) {
			c.sendAck(env.AckID)
			return
		}
		if _, ciErr := c.server.runs.CancelInactiveCtx(c.authCtx(), c.runID); ciErr != nil && c.server.logger != nil {
			c.server.logger.Warn("server: ws cancel of inactive run %s: %v", c.runID, ciErr)
		}
		err = nil
	}
	if err != nil {
		c.sendError("cancel_failed", err.Error(), env.AckID)
		return
	}
	c.sendAck(env.AckID)
}

// handlePause is the WS counterpart of POST /api/runs/{id}/pause.
// Soft-pause: signals the engine to interrupt at the next safe
// boundary, save a checkpoint, and transition to paused_operator —
// resumable like a cancelled run. Idempotent.
func (c *runConn) handlePause(env runWSEnvelope) {
	if c.xStore != nil {
		c.sendError("cross_store_readonly", "pause is not available for cross-store runs — open the owning daemon to pause", env.AckID)
		return
	}
	if c.server.logger != nil {
		c.server.logger.Info("server: pause run %q via WS from %s", c.runID, c.conn.RemoteAddr())
	}
	if err := c.server.runs.Pause(c.runID); err != nil {
		if errors.Is(err, runview.ErrRunNotActive) {
			// The run isn't held in this process — either terminal or
			// running cross-process (cloud). Studio hides the Pause
			// button in those cases; this protects against races.
			c.sendError("not_active", "run is not active in this process", env.AckID)
			return
		}
		c.sendError("pause_failed", err.Error(), env.AckID)
		return
	}
	c.sendAck(env.AckID)
}

func (c *runConn) handleAnswer(env runWSEnvelope) {
	if c.xStore != nil {
		c.sendError("cross_store_readonly", "answer is not available for cross-store runs — open the owning daemon to answer", env.AckID)
		return
	}
	// An answer resumes the run: runs.Resume below re-enters the engine
	// (node execution + budget/cost spend), so it is a launch for
	// admission purposes — the FULL gate (suspend, concurrency, launch
	// rate, cost cap, monthly run quota), exactly like handleResumeRun,
	// else the WS answer surface bypasses the org quotas the REST paths
	// enforce. The auth identity is the one snapshotted at upgrade, NOT
	// authCtx() (which only carries the store tenant tag) — re-stamped
	// here so gateLaunch sees it.
	if _, d := c.server.gateLaunch(auth.WithIdentity(c.authCtx(), c.identity)); d != nil {
		c.sendError(d.reason, d.detail, env.AckID)
		return
	}
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
		runMeta, err := c.server.runs.LoadRunCtx(c.authCtx(), c.runID)
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
	// Use authCtx (Background-derived, carries tenant/user identity) so
	// closing the browser tab doesn't cancel the resume but the mongo
	// tenant_id filter still applies on writes.
	if _, err := c.server.runs.Resume(c.authCtx(), runview.ResumeSpec{
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

func (c *runConn) handleQueueMessage(env runWSEnvelope) {
	if c.xStore != nil {
		c.sendError("cross_store_readonly", "queue-message is not available for cross-store runs", env.AckID)
		return
	}
	var req wsQueueMessageRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		c.sendError("bad_payload", err.Error(), env.AckID)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		c.sendError("empty_message", "text is required", env.AckID)
		return
	}
	var qopts []runview.QueueMessageOption
	if len(req.Skills) > 0 {
		qopts = append(qopts, runview.WithMessageSkills(req.Skills))
	}
	msg, err := c.server.runs.QueueMessage(c.authCtx(), c.runID, req.Text, qopts...)
	if err != nil {
		c.sendError("queue_failed", err.Error(), env.AckID)
		return
	}
	c.sendEnvelope(wsTypeAck, msg, env.AckID)
}

func (c *runConn) handleCancelQueuedMessage(env runWSEnvelope) {
	if c.xStore != nil {
		c.sendError("cross_store_readonly", "cancel-queued-message is not available for cross-store runs", env.AckID)
		return
	}
	var req wsCancelQueuedMessageRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		c.sendError("bad_payload", err.Error(), env.AckID)
		return
	}
	if req.MessageID == "" {
		c.sendError("missing_message_id", "message_id is required", env.AckID)
		return
	}
	if err := c.server.runs.CancelQueuedMessage(c.authCtx(), c.runID, req.MessageID); err != nil {
		switch {
		case errors.Is(err, store.ErrQueuedMessageNotFound):
			c.sendError("not_found", err.Error(), env.AckID)
		case errors.Is(err, store.ErrQueuedMessageStatusConflict):
			c.sendError("status_conflict", err.Error(), env.AckID)
		default:
			c.sendError("cancel_failed", err.Error(), env.AckID)
		}
		return
	}
	c.sendAck(env.AckID)
}

// sendEnvelope marshals and queues a server→client envelope. Returns
// false if the connection is being torn down.
func (c *runConn) sendEnvelope(t string, payload any, ackID string) bool {
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
	// Drop-on-full to avoid pinning the broker goroutine on a slow
	// (frozen browser tab, throttled connection) client. The blocking
	// send would otherwise hold up to writeWait per stalled write
	// before the write deadline fires — fine for one client, but
	// accumulates badly under many parked tabs. The SPA's reconnect
	// path re-anchors the run so a closed connection here is not data
	// loss for the user.
	select {
	case c.sendCh <- data:
		return true
	case <-c.closed:
		return false
	default:
		c.server.logger.Warn("ws: send buffer full for run %s — closing slow consumer", c.runID)
		c.close()
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
