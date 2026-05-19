package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/runview"
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
	FilePath string                 `json:"file_path,omitempty"` // optional; falls back to run.FilePath
	Source   string                 `json:"source,omitempty"`    // see resumeRunRequest.Source
	Answers  map[string]interface{} `json:"answers"`
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

	mu            sync.Mutex
	subscribed    bool
	sub           *runview.EventSubscription
	logSubscribed bool
	logSub        *runview.RunLogSubscription
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
	case wsTypePause:
		c.handlePause(env)
	case wsTypeAnswer:
		c.handleAnswer(env)
	case wsTypeQueueMessage:
		c.handleQueueMessage(env)
	case wsTypeCancelQueuedMessage:
		c.handleCancelQueuedMessage(env)
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
	// Cross-store mode skips the in-process broker entirely (the
	// foreign run's events are persisted by a different daemon and
	// never reach this process's broker). We tail the foreign
	// events.jsonl directly via streamEventsCrossStore below.
	//
	// In cloud mode (Mongo change-stream source wired) we also skip
	// the broker — the Mongo source handles both historical replay
	// and live tail in a single Subscribe call.
	//
	// In local same-store mode we keep the broker so multi-WS clients
	// on the same process share the same fan-out cursor.
	if c.xStore == nil && !c.server.runs.HasEventSource() {
		c.sub = c.server.runs.Broker().Subscribe(c.runID)
	}
	c.subscribed = true
	c.mu.Unlock()

	var snap *runview.RunSnapshot
	var err error
	if c.xStore != nil {
		snap, err = runview.BuildSnapshot(context.Background(), c.xStore, c.runID)
	} else {
		snap, err = c.server.runs.SnapshotCtx(c.authCtx(), c.runID)
	}
	if err != nil {
		c.sendError("snapshot_failed", err.Error(), env.AckID)
		return
	}
	c.sendEnvelope(wsTypeSnapshot, snap, env.AckID)

	// Lazy mode: advance the effective replay floor past the snapshot
	// so both the local pagination loop and the cloud event-source
	// see "nothing to replay". Live tail still flows unaffected. The
	// frontend pulls history on demand via /api/runs/{id}/events.
	effectiveFromSeq := req.FromSeq
	if !req.ReplayHistory && snap.LastSeq != runview.NoEventsSeq {
		effectiveFromSeq = snap.LastSeq + 1
	}
	go c.streamEvents(effectiveFromSeq, snap.LastSeq)
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
	if c.xStore != nil {
		c.streamEventsCrossStore(fromSeq, snapshotSeq)
		return
	}
	if c.server.runs.HasEventSource() {
		c.streamEventsCloud(fromSeq)
		return
	}
	c.streamEventsLocal(fromSeq, snapshotSeq)
}

// streamEventsCrossStore is the read-only cross-store path: replay
// historical events from the foreign store's events.jsonl, then tail
// the file via fsnotify (with a polling fallback) for new events. The
// foreign run is being driven by a different daemon or CLI process —
// this daemon has no broker subscription for it, but the file is the
// authoritative source of truth and live-tailing it gives the WS
// client the same live-update UX as in-process subscriptions.
//
// Terminal detection: the loop watches the run.json status alongside
// the events file. Once the status reaches a terminal value
// (finished / failed / cancelled) the function sends wsTypeTerminated
// and returns. Without this, the tail would run forever on a finished
// run (the file simply stops growing, which is indistinguishable from
// "still working" by file watch alone).
func (c *runConn) streamEventsCrossStore(fromSeq, snapshotSeq int64) {
	if c.xStorePath == "" {
		c.sendError("cross_store_unconfigured", "xStorePath empty", "")
		return
	}

	// 1. Replay historical events [fromSeq, snapshotSeq+1) via the
	//    foreign store's paginated range API.
	if snapshotSeq != runview.NoEventsSeq && (fromSeq > 0 || snapshotSeq > 0) {
		next := fromSeq
		for {
			events, err := c.xStore.LoadEventsRange(context.Background(), c.runID, next, snapshotSeq+1, runview.MaxEventsPerPage)
			if err != nil {
				c.server.logger.Warn("runs_ws: cross-store replay (%s): %v", c.runID, err)
				break
			}
			if len(events) > 0 {
				if !c.sendEnvelope(wsTypeEventBatch, events, "") {
					return
				}
			}
			if len(events) < runview.MaxEventsPerPage {
				break
			}
			next = events[len(events)-1].Seq + 1
			if next > snapshotSeq {
				break
			}
		}
	}

	// 2. Tail the foreign events.jsonl for new appends. Snapshot the
	//    last replayed seq so the tail dedups any overlap with the
	//    replay window above (events written between the snapshot
	//    moment and now).
	lastSeq := snapshotSeq
	c.tailCrossStoreEvents(&lastSeq)
}

// crossStoreTailPollInterval is the wide defensive poll cadence —
// fsnotify is the fast path; the polling tier catches missed events
// when the inotify queue overflows under high-throughput appends.
const crossStoreTailPollInterval = 2 * time.Second

// crossStoreTerminalCheckInterval bounds how often we re-read run.json
// to check for terminal status. Cheap — it's a tiny file — but no need
// to re-stat every event.
const crossStoreTerminalCheckInterval = 5 * time.Second

// tailCrossStoreEvents is the long-running tail loop. Reads any bytes
// appended past the recorded offset, parses each complete line as a
// store.Event, dedups by seq against *lastSeq, ships via wsTypeEvent.
// Returns when c.closed fires or the run reaches a terminal status.
func (c *runConn) tailCrossStoreEvents(lastSeq *int64) {
	eventsPath := filepath.Join(c.xStorePath, "runs", c.runID, "events.jsonl")

	watcher, watcherErr := fsnotify.NewWatcher()
	if watcherErr != nil {
		c.server.logger.Warn("runs_ws: cross-store tail (%s): fsnotify unavailable, polling: %v", c.runID, watcherErr)
		c.tailCrossStoreEventsPolling(eventsPath, lastSeq)
		return
	}
	defer watcher.Close()

	dir := filepath.Dir(eventsPath)
	if err := watcher.Add(dir); err != nil {
		c.server.logger.Warn("runs_ws: cross-store tail (%s): watcher.Add(%q): %v — polling", c.runID, dir, err)
		c.tailCrossStoreEventsPolling(eventsPath, lastSeq)
		return
	}

	var offset int64
	offset = c.drainNewCrossStoreEvents(eventsPath, offset, lastSeq)

	pollTicker := time.NewTicker(crossStoreTailPollInterval)
	defer pollTicker.Stop()
	terminalTicker := time.NewTicker(crossStoreTerminalCheckInterval)
	defer terminalTicker.Stop()

	for {
		select {
		case <-c.closed:
			return
		case <-terminalTicker.C:
			if c.checkCrossStoreTerminal(eventsPath, &offset, lastSeq) {
				return
			}
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) != filepath.Clean(eventsPath) {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				offset = c.drainNewCrossStoreEvents(eventsPath, offset, lastSeq)
			}
		case <-pollTicker.C:
			offset = c.drainNewCrossStoreEvents(eventsPath, offset, lastSeq)
		case err := <-watcher.Errors:
			c.server.logger.Warn("runs_ws: cross-store tail (%s): watcher error: %v", c.runID, err)
		}
	}
}

// tailCrossStoreEventsPolling is the fsnotify-less fallback (rare —
// only fires on hosts where inotify isn't available, e.g. some
// container filesystems).
func (c *runConn) tailCrossStoreEventsPolling(eventsPath string, lastSeq *int64) {
	var offset int64
	offset = c.drainNewCrossStoreEvents(eventsPath, offset, lastSeq)

	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()
	terminalTicker := time.NewTicker(crossStoreTerminalCheckInterval)
	defer terminalTicker.Stop()

	for {
		select {
		case <-c.closed:
			return
		case <-terminalTicker.C:
			if c.checkCrossStoreTerminal(eventsPath, &offset, lastSeq) {
				return
			}
		case <-pollTicker.C:
			offset = c.drainNewCrossStoreEvents(eventsPath, offset, lastSeq)
		}
	}
}

// drainNewCrossStoreEvents reads bytes appended past `offset`, splits
// on newlines, parses each complete line as a store.Event, sends it
// to the WS (deduped against *lastSeq). Trailing partial line bytes
// stay in the file for the next call — the returned offset advances
// only past COMPLETE lines so we never lose a half-written event when
// the producer flushes mid-line.
//
// File truncation / rotation: if Stat shows the file is shorter than
// the recorded offset (rare; happens if the producer rewrites the
// file from scratch on resume), we reset to 0 and replay everything.
// The dedup-by-seq logic above ensures the WS client doesn't see
// duplicates from the replay window.
func (c *runConn) drainNewCrossStoreEvents(eventsPath string, offset int64, lastSeq *int64) int64 {
	f, err := os.Open(eventsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.server.logger.Warn("runs_ws: cross-store tail (%s): open: %v", c.runID, err)
		}
		return offset
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return offset
	}
	if st.Size() < offset {
		// File was rotated / truncated — restart from the top.
		offset = 0
	}
	if st.Size() == offset {
		return offset // nothing new
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		c.server.logger.Warn("runs_ws: cross-store tail (%s): seek %d: %v", c.runID, offset, err)
		return offset
	}

	// Read all remaining bytes (typically a few KB per drain — events
	// are tiny JSON lines). Sidecar blobs (large tool I/O) live in
	// runs/<id>/tools/, not inline in events.jsonl, so this stays
	// bounded.
	body, err := io.ReadAll(f)
	if err != nil {
		c.server.logger.Warn("runs_ws: cross-store tail (%s): read: %v", c.runID, err)
		return offset
	}

	// Find the last newline; bytes past it are a partial line we'll
	// pick up on the next drain.
	lastNL := -1
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == '\n' {
			lastNL = i
			break
		}
	}
	if lastNL < 0 {
		// All accumulated bytes are still a partial line — don't
		// advance offset.
		return offset
	}
	complete := body[:lastNL+1]

	// Process each complete line.
	start := 0
	for i := 0; i < len(complete); i++ {
		if complete[i] != '\n' {
			continue
		}
		line := complete[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var ev store.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			c.server.logger.Warn("runs_ws: cross-store tail (%s): bad event line: %v", c.runID, err)
			continue
		}
		if ev.Seq <= *lastSeq {
			continue
		}
		if !c.sendEnvelope(wsTypeEvent, &ev, "") {
			return offset + int64(len(complete))
		}
		*lastSeq = ev.Seq
	}
	return offset + int64(len(complete))
}

// checkCrossStoreTerminal re-reads run.json and, if the status is
// terminal, drains any final events from the file, sends wsTypeTerminated,
// and returns true. The caller stops tailing in that case.
func (c *runConn) checkCrossStoreTerminal(eventsPath string, offset *int64, lastSeq *int64) bool {
	run, err := c.xStore.LoadRun(context.Background(), c.runID)
	if err != nil {
		return false
	}
	switch run.Status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		*offset = c.drainNewCrossStoreEvents(eventsPath, *offset, lastSeq)
		c.sendEnvelope(wsTypeTerminated, map[string]string{"run_id": c.runID}, "")
		return true
	}
	return false
}

// streamEventsLocal is the original broker-backed path: replay disk
// events [fromSeq, snapshotSeq+1) then drain the broker channel,
// dedup'ing against the replay window.
//
// LoadEvents caps at runview.MaxEventsPerPage so for runs whose
// replay window exceeds the cap we MUST paginate — otherwise the
// tail of the events stream silently disappears, including any
// terminal run_failed/run_finished events, which leaves the
// studio's status pill stuck on whatever pre-terminal status the
// last replayed event implied.
func (c *runConn) streamEventsLocal(fromSeq, snapshotSeq int64) {
	if snapshotSeq != runview.NoEventsSeq && (fromSeq > 0 || snapshotSeq > 0) {
		next := fromSeq
		for {
			events, err := c.server.runs.LoadEventsCtx(c.authCtx(), c.runID, next, snapshotSeq+1)
			// LoadEventsCtx returns both partial events AND
			// ErrEventsCorrupted when a run's events.jsonl is too
			// damaged to trust. Surface that to the client as an
			// error envelope BEFORE breaking so the studio can
			// raise a banner instead of silently rendering a
			// truncated history as if it were complete.
			if errors.Is(err, store.ErrEventsCorrupted) {
				if len(events) > 0 {
					_ = c.sendEnvelope(wsTypeEventBatch, events, "")
				}
				c.sendError("events_corrupted", err.Error(), "")
				return
			}
			if err != nil {
				break
			}
			if len(events) > 0 {
				// Ship the whole page as one batch envelope: cuts
				// per-envelope marshal + WS-frame overhead by the
				// page size (up to MaxEventsPerPage×). The frontend
				// dispatches one state update per page instead of
				// one per event.
				if !c.sendEnvelope(wsTypeEventBatch, events, "") {
					return
				}
			}
			if len(events) < runview.MaxEventsPerPage {
				break
			}
			next = events[len(events)-1].Seq + 1
			if next > snapshotSeq {
				break
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
	sub, err := c.server.runs.SubscribeEventStream(c.authCtx(), c.runID, fromSeq)
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
		// Match the HTTP handler's behaviour: when no goroutine owns
		// the run, the operator likely wants to abandon a paused or
		// failed_resumable run. CancelInactive flips status to
		// cancelled + runs RecoverFinalize so the studio's merge UI
		// surfaces whatever commits the run produced. Already-terminal
		// statuses are a silent no-op.
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
