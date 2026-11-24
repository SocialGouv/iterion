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
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
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

// defaultRunWSMaxLifetime bounds how long one run socket stays open, whatever
// its traffic. The identity is read once, at the upgrade, and a subscription
// needs no further envelope, so without this cap the run's event stream
// outlives its credential's account for as long as the client answers the
// pings (the mutating envelopes carry their own question — runConn.standing).
// It is the post-mortem shell's absolute cap (runs_shell.go), so the two
// sockets of the same console state one truth; the cost of reaching it is one
// redial, which the studio does by itself and resumes from its last seq.
const defaultRunWSMaxLifetime = 2 * time.Hour

// runWSStandingTimeout bounds the standing read one mutating envelope waits
// for. The read pump serves envelopes one at a time, so a store that never
// answers would hold the whole command surface of the socket.
const runWSStandingTimeout = 10 * time.Second

// runWSMaxLifetime is defaultRunWSMaxLifetime, overridable by the operator
// like the shell's caps. A deployment that wants no practical cap states a
// long duration (ITERION_RUN_WS_MAX_LIFETIME=87600h).
func runWSMaxLifetime() time.Duration {
	return envDurationOrDefault("ITERION_RUN_WS_MAX_LIFETIME", defaultRunWSMaxLifetime)
}

// runWSStandingTickPeriod is how often a SUBSCRIBED passive connection
// re-derives its standing. The ADR-148 review's F1: subscribe and
// subscribe_logs carry no envelope of their own, so without a tick a
// stream would outlive the standing it was opened under until the
// lifetime cap. Overridable like the lifetime cap; the standing read
// inside a tick is bounded by runWSStandingTimeout.
const defaultRunWSStandingTick = 5 * time.Minute

func runWSStandingTickPeriod() time.Duration {
	return envDurationOrDefault("ITERION_RUN_WS_STANDING_TICK", defaultRunWSStandingTick)
}

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
// the iterion home, store.IterionHome), the subscription reads snapshots + tails events
// from THAT store instead of the daemon's primary. State-changing
// commands (cancel, resume, answer) are rejected with cross_store_readonly
// in this mode since we don't drive the foreign run's engine — its
// owning daemon (or CLI process) does.
func (s *Server) handleRunWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.runs == nil {
		httpError(w, http.StatusServiceUnavailable, "run console not configured")
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
	// gated the path under the iterion home.
	//
	// The run read here is also the ORIGIN the socket keeps: its header is
	// what every snapshot, event and log subscription of this socket binds
	// to (runConn.streamCtx), so a run deleted and recreated under the same
	// ID is refused rather than served in its place. The cross-store read
	// binds the same way; a run absent from the foreign store stays
	// unbound, as before, so one created after the upgrade is admitted.
	//
	// A dial carrying an expected birth was compared by runOriginGate: the
	// socket binds exactly the origin the gate compared, without a second
	// read a successor could slip into — and a redial carries its
	// expectation again, on whichever replica serves it.
	var origin store.RunObservation
	admittedOnExpectation := false
	if admitted, ok := admittedOrigin(r.Context(), runID); ok {
		origin, admittedOnExpectation = admitted, true
	} else if xStore == nil {
		o, lerr := s.runs.CaptureRunOrigin(r.Context(), runID)
		if lerr != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		origin = o
	} else {
		o, lerr := runview.CaptureRunOriginIn(r.Context(), xStore, runID)
		switch {
		case lerr == nil:
			origin = o
		case errors.Is(lerr, store.ErrRunNotFound):
		case errors.Is(lerr, store.ErrRunDeleted):
			http.Error(w, "run deleted", http.StatusGone)
			return
		default:
			http.Error(w, "cross-store run could not be read: "+lerr.Error(), http.StatusInternalServerError)
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
	rc.clientIP = s.clientIP(r)
	rc.xStore = xStore
	rc.xStorePath = xStorePath
	rc.origin = origin
	rc.admittedOnExpectation = admittedOnExpectation
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

	// clientIP snapshots the address clientIP named for the upgrade
	// request: the WS has no *http.Request afterwards, and the kernel's
	// peer address would make one operator read two identities for one
	// client — the forwarded address in the audit row, the ingress pod
	// and its source port in the run-control log (#2673).
	clientIP string

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

	// origin is the run the upgrade authorized, observed once by the
	// upgrade's own read: header and stored tenant, provenance only. Zero
	// (unobserved) when a cross-store run was absent at the upgrade.
	origin store.RunObservation
	// admittedOnExpectation reports that origin is the one runOriginGate
	// compared with the dial's expected birth: the socket's mutating actions
	// then run bound to it (actionContext). A socket dialed without an
	// expectation keeps the unbound actions every older client gets.
	admittedOnExpectation bool

	mu                  sync.Mutex
	eventOwner          *wsSubscriptionOwner
	logOwner            *wsSubscriptionOwner
	subscriptionWorkers sync.WaitGroup
	closeOnce           sync.Once
	closed              chan struct{}
	// closeFrame is what the write pump writes when it reads the empty item
	// sendCh carries as the close marker. Set once — the first reason is the
	// one that decided — under mu, which the pump takes to read it.
	closeFrame *wsCloseFrame
}

// wsCloseMarker is the empty item sendCh carries to tell the write pump that
// the next thing to write is runConn.closeFrame. It rides the SAME channel as
// the envelopes, so the close is ordered BEHIND everything the server decided
// before it — the error naming a refusal reaches the client by FIFO, not by
// an interleaving a reader would have to reconstruct. A marshalled envelope
// is never empty (json.Marshal of a struct yields at least two bytes), so the
// marker is unambiguous.
var wsCloseMarker []byte

// wsCloseFrame is a close code and the reason behind it.
type wsCloseFrame struct {
	code  int
	token string
	cause string
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

// streamCtx is authCtx bound to the run the upgrade authorized
// (store.BindRunObservation): the parent of every event and log reservation,
// so a snapshot, replay or log read of any (re)subscription reads that run
// and is refused for a successor recreated under the same ID. On a socket
// admitted on an expected birth it is also the context of the mutating
// actions that only touch the store (actionContext); a resume or a child run
// never inherits it. A connection with no observed origin (a cross-store run
// absent at the upgrade) stays unbound.
func (c *runConn) streamCtx() (context.Context, error) {
	ctx := c.authCtx()
	if !c.origin.Observed() {
		return ctx, nil
	}
	return store.BindRunObservation(ctx, c.origin)
}

// wsErrRunOriginLost answers a mutating envelope whose socket's origin is
// gone: the run behind its ID was replaced or deleted after the upgrade.
const wsErrRunOriginLost = "run_origin_lost"

// wsCauseOriginLost is the cause the scope_lost close frame names when the
// socket's origin is gone.
const wsCauseOriginLost = "the run this socket watches was replaced or deleted"

// runOriginLost reports whether err says the run behind the ID is no longer
// the origin the reader bound: a successor's header, a deletion, an absence.
func runOriginLost(err error) bool {
	return errors.Is(err, store.ErrRunDataIdentityChanged) || errors.Is(err, store.ErrRunDeleted) || errors.Is(err, store.ErrRunNotFound)
}

// closeOnOriginLost closes a bound connection whose origin err reports gone,
// behind the typed error envelope the caller already queued: nothing this
// socket was opened on can be served again, and a redial authorizes whatever
// run holds the ID now. An unbound connection, or any other error, keeps the
// socket.
func (c *runConn) closeOnOriginLost(err error) {
	if !c.origin.Observed() || !runOriginLost(err) {
		return
	}
	if c.server != nil && c.server.logger != nil {
		c.server.logger.Info("server: ws of run %s lost its origin — closing: %v", c.runID, err)
	}
	c.closeWithReason(websocket.CloseNormalClosure, wsCloseScopeLost, wsCauseOriginLost)
}

// actionContext is the context a mutating envelope's action runs under. On
// a socket the gate admitted on an expected birth — unless the operator
// turned the comparison off (ITERION_RUN_BIRTH_CHECK=0) — it is the socket's
// bound context (streamCtx), checked: the action's store accesses refuse a
// successor recreated under the ID, and a store that may drop the context is
// refused. Otherwise it is the unbound authCtx, unchecked: exactly the calls
// an older client's socket makes.
func (c *runConn) actionContext() (ctx context.Context, checked bool, err error) {
	if !c.admittedOnExpectation || runBirthCheckOff() {
		return c.authCtx(), false, nil
	}
	if err := store.RequireBoundObservationStore(c.server.runs.RunStore()); err != nil {
		return nil, false, err
	}
	ctx, err = c.streamCtx()
	if err != nil {
		return nil, false, err
	}
	return ctx, true, nil
}

// actionExpectation is what an envelope that resumes the run carries into
// the run service instead of a bound context (runview.ActionOrigin): the
// socket's origin, checked on the sockets actionContext checks — the service
// verifies the run it re-enters and the execution keeps no binding — and the
// zero value on every other socket. A store that may drop the context is
// refused as actionContext refuses it.
func (c *runConn) actionExpectation() (runview.ActionOrigin, error) {
	_, checked, err := c.actionContext()
	if err != nil || !checked {
		return runview.ActionOrigin{}, err
	}
	return runview.ExpectRunOrigin(c.origin), nil
}

// sendScopeUnavailable answers an envelope whose action could not be bound
// to the socket's origin (actionContext): the action does not run.
func (c *runConn) sendScopeUnavailable(env runWSEnvelope, err error) {
	c.sendError(errCodeScopeUnavailable, "the run this socket watches cannot be bound to its origin: "+err.Error(), env.AckID)
}

// writeActionFailure answers a checked envelope's failure by the rule of the
// HTTP actions (Server.writeActionFailure): it reads the run's current
// origin, unbound, and only when the run is gone or holds another birth than
// the socket's sends run_origin_lost and closes the socket scope_lost. It
// reports whether it answered; otherwise the envelope's own handling applies.
func (c *runConn) writeActionFailure(env runWSEnvelope, err error) bool {
	if err == nil || !c.origin.Observed() {
		return false
	}
	current, rerr := c.server.runs.CaptureRunOrigin(c.authCtx(), c.runID)
	switch {
	case rerr == nil && current.Header().BirthID == c.origin.Header().BirthID:
		return false
	case rerr == nil || runOriginLost(rerr):
	default:
		if c.server.logger != nil {
			c.server.logger.Warn("server: ws of run %s: a %s failed (%v) and its origin could not be read again: %v", c.runID, env.Type, err, rerr)
		}
		return false
	}
	if c.server.logger != nil {
		c.server.logger.Warn("server: ws of run %s: a %s admitted at birth %s failed on a lost origin: %v", c.runID, env.Type, quoteBirth(c.origin.Header().BirthID), err)
	}
	c.sendError(wsErrRunOriginLost, wsCauseOriginLost+" — reopen it to act on the run that holds this ID now", env.AckID)
	c.closeWithReason(websocket.CloseNormalClosure, wsCloseScopeLost, wsCauseOriginLost)
	return true
}

// refuseChecked answers an envelope's refusal: on a checked envelope by the
// failure rule first (writeActionFailure — the refusal may have judged the
// run that holds the ID now, not the socket's origin), otherwise, or when
// the origin holds, with code and msg.
func (c *runConn) refuseChecked(env runWSEnvelope, checked bool, cause error, code, msg string) {
	if checked && c.writeActionFailure(env, cause) {
		return
	}
	c.sendError(code, msg, env.AckID)
}

// requireBoundSource refuses, before any subscription, a stream source that
// cannot honor the bound context of a connection with an origin: stripping
// the binding would serve whatever run holds the ID now.
func (c *runConn) requireBoundSource() error {
	if !c.origin.Observed() {
		return nil
	}
	return runstream.RequireBoundObservationSource(c.src)
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
	defer c.finishConnection()
	errtrack.Go("server.runWS.writePump", c.writePump)
	errtrack.Go("server.runWS.watchLifetime", c.watchLifetime)
	errtrack.Go("server.runWS.watchStanding", c.watchStanding)
	c.readPump()
}

// watchLifetime closes the socket when its absolute lifetime is up
// (runWSMaxLifetime). The client is told which bound it hit, so it redials
// instead of treating the close as a transport failure.
func (c *runConn) watchLifetime() {
	lifetime := runWSMaxLifetime()
	t := time.NewTimer(lifetime)
	defer t.Stop()
	select {
	case <-c.closed:
	case <-t.C:
		if c.server.logger != nil {
			c.server.logger.Info("server: ws of run %s reached its lifetime cap (%s) — closing", c.runID, lifetime)
		}
		c.closeWithReason(websocket.CloseNormalClosure, wsCloseLifetimeReached, "reopen the socket to present a credential again")
	}
}

// watchStanding is the passive connection's half of the standing rule.
// subscribe and subscribe_logs carry no envelope of their own, so the
// mutating-envelope gate never re-derives their scope. Every tick, a
// SUBSCRIBED connection re-asks what the upgrade answered once — does the
// credential still stand, and does a viewer standing in the run's team
// still hold? The run-visibility axis is part of that standing (ADR-148):
// closing it, or removing the org-membership it leaned on, ends the
// stream within one tick. An unreadable read is no verdict — the socket
// stays and the failure is logged; the server's own failure is no
// refusal. A connection no auth context ever reached (the local run
// console) has nothing to re-derive.
func (c *runConn) watchStanding() {
	if c.identity.Credential == "" {
		return
	}
	// A cross-store connection (?store=) watches a FOREIGN store's run:
	// runTenant below reads the primary store and would answer not-found,
	// closing the socket at the first tick with a reason that names no
	// bound — the same exemption the mutating-envelope gate makes for
	// c.xStore (the ADR-148 review's R63ff56).
	if c.xStore != nil {
		return
	}
	t := time.NewTicker(runWSStandingTickPeriod())
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
		}
		c.mu.Lock()
		idle := c.eventOwner == nil && c.logOwner == nil
		c.mu.Unlock()
		if idle {
			continue
		}
		ctx, cancel := context.WithTimeout(c.authCtx(), runWSStandingTimeout)
		user, err := c.server.checkStanding(ctx, c.identity)
		if err != nil {
			cancel()
			if mapAuthErrorStatus(err) >= http.StatusInternalServerError {
				if c.server.logger != nil {
					c.server.logger.Error("server: ws of run %s: the standing of its credential could not be read: %v", c.runID, err)
				}
				continue
			}
			c.closeWithReason(websocket.ClosePolicyViolation, wsCloseCredentialRefused, err.Error())
			return
		}
		scoped := c.identity
		scoped.IsSuperAdmin = user.IsSuperAdmin
		team, terr := c.server.runTenant(ctx, c.runID)
		if terr != nil {
			cancel()
			if errors.Is(terr, store.ErrRunNotFound) || errors.Is(terr, store.ErrRunDeleted) {
				c.closeWithReason(websocket.CloseNormalClosure, wsCloseScopeLost, "the run this socket watches is gone")
				return
			}
			if c.server.logger != nil {
				c.server.logger.Error("server: ws of run %s: its team could not be resolved: %v", c.runID, terr)
			}
			continue
		}
		resolved, ok, serr := identityInTeamOn(c.server.authStore(), ctx, scoped, team, true)
		cancel()
		if serr != nil {
			if c.server.logger != nil {
				c.server.logger.Error("server: ws of run %s: the caller's standing in %s could not be read: %v", c.runID, team, serr)
			}
			continue
		}
		if !ok || !resolved.Role.AtLeast(identity.RoleViewer) {
			c.closeWithReason(websocket.ClosePolicyViolation, wsCloseScopeLost, "your standing in the run's team no longer holds")
			return
		}
	}
}

// closeWithReason queues the close frame behind the envelopes the server has
// already decided, so the client reads them before the socket goes down; the
// write pump tears the connection down once the frame is on the wire. It
// returns at once if the connection is already gone — a full buffer means
// sendEnvelope is closing a consumer that stopped reading.
func (c *runConn) closeWithReason(code int, token, cause string) {
	c.mu.Lock()
	if c.closeFrame == nil {
		c.closeFrame = &wsCloseFrame{code: code, token: token, cause: cause}
	}
	c.mu.Unlock()
	select {
	case c.sendCh <- wsCloseMarker:
	case <-c.closed:
	}
}

func (c *runConn) close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		close(c.closed)
		events, logs := c.eventOwner, c.logOwner
		c.eventOwner, c.logOwner = nil, nil
		c.mu.Unlock()
		if events != nil {
			events.cancel()
		}
		if logs != nil {
			logs.cancel()
		}
		if c.conn != nil {
			_ = c.conn.Close()
		}
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

// runWSMutatingTypes are the envelopes that change the run (the review's
// R571177): the ladder's act rung applies to them exactly as it would to
// the same command over HTTP — a read-only viewer connection cannot
// cancel, pause, answer or steer the run through the event socket.
var runWSMutatingTypes = map[string]bool{
	wsTypeCancel:              true,
	wsTypePause:               true,
	wsTypeAnswer:              true,
	wsTypeQueueMessage:        true,
	wsTypeCancelQueuedMessage: true,
	wsTypeBumpLoop:            true,
	wsTypeRaiseBudget:         true,
}

func (c *runConn) dispatch(env runWSEnvelope) {
	if runWSMutatingTypes[env.Type] {
		if !c.mutationAllowed(env) {
			return
		}
	}
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

// mutationAllowed asks, per envelope that changes the run, whether the
// credential this socket was opened with still stands AND still carries this
// run. The whole scope — the run's team, the caller's standing THERE, the
// super-admin reach — was resolved once at the upgrade and is re-derived on
// every mutating envelope from the account as the store reads it now: a
// membership removed or a super-admin demoted after the upgrade stops
// steering the run at the next envelope instead of at the next upgrade, and
// a socket stops at its credential's expiry-side refusals the same way.
//
// A refusal of the CREDENTIAL answers the envelope and closes the socket,
// naming the bound in the close frame: nothing this credential sends will be
// served again. A refusal of the SCOPE answers the envelope forbidden and
// leaves the socket's reads alone. A read that did not complete refuses THAT
// envelope only — the server's own failure is no verdict on the credential
// (mapAuthErrorStatus tells the two apart).
//
// A connection carrying no credential is one no auth context ever reached
// (the local run console, a server built without the middleware): there is
// no credential to ask about, but its origin is still compared (originHolds).
// A cross-store connection drives no run of this server (its mutating
// envelopes are refused downstream), so neither its origin nor its scope is
// re-derived here; its credential still is.
func (c *runConn) mutationAllowed(env runWSEnvelope) bool {
	if c.identity.Credential == "" {
		return c.originHolds(env)
	}
	ctx, cancel := context.WithTimeout(c.authCtx(), runWSStandingTimeout)
	defer cancel()
	user, err := c.server.checkStanding(ctx, c.identity)
	if err != nil {
		if mapAuthErrorStatus(err) >= http.StatusInternalServerError {
			if c.server.logger != nil {
				c.server.logger.Error("server: ws of run %s: the standing of its credential could not be read: %v", c.runID, err)
			}
			c.sendError("standing_unavailable", "the credential of this connection could not be checked — retry", env.AckID)
			return false
		}
		if c.server.logger != nil {
			c.server.logger.Warn("server: ws of run %s refused a %s and closed: %v", c.runID, env.Type, err)
		}
		c.sendError(wsCloseCredentialRefused, err.Error(), env.AckID)
		c.closeWithReason(websocket.ClosePolicyViolation, wsCloseCredentialRefused, err.Error())
		return false
	}
	if c.xStore != nil {
		return true
	}
	if !c.originHolds(env) {
		return false
	}
	team, terr := c.server.runTenant(ctx, c.runID)
	if terr != nil {
		if errors.Is(terr, store.ErrRunNotFound) || errors.Is(terr, store.ErrRunDeleted) {
			c.sendError("forbidden", "the run this socket watches is gone", env.AckID)
			return false
		}
		if c.server.logger != nil {
			c.server.logger.Error("server: ws of run %s: its team could not be resolved: %v", c.runID, terr)
		}
		c.sendError("scope_unavailable", "the run's team could not be resolved — retry", env.AckID)
		return false
	}
	envID := c.identity
	envID.IsSuperAdmin = user.IsSuperAdmin
	resolved, ok, serr := identityInTeamOn(c.server.authStore(), ctx, envID, team, true)
	if serr != nil {
		if c.server.logger != nil {
			c.server.logger.Error("server: ws of run %s: the caller's standing in %s could not be read: %v", c.runID, team, serr)
		}
		c.sendError("scope_unavailable", "your standing in the run's team could not be resolved — retry", env.AckID)
		return false
	}
	if !ok || !resolved.Role.AtLeast(identity.RoleMember) {
		if c.server.logger != nil {
			c.server.logger.Warn("server: ws of run %s refused a %s: no member standing in %s", c.runID, env.Type, team)
		}
		c.sendError("forbidden", "your standing in the run's team no longer allows this action", env.AckID)
		return false
	}
	return true
}

// originHolds asks, per envelope that changes the run, whether the run behind
// this socket's ID is still the origin its upgrade authorized: the run is
// loaded under the bound context (streamCtx), which a native store refuses for
// a successor recreated under the same ID. A replaced, deleted or missing
// origin refuses the envelope with run_origin_lost and closes the socket
// scope_lost — the run it was opened on is gone, and the action would land on
// another. A read that did not complete refuses THAT envelope only.
//
// This is an admission check, not an atomic guarantee: the run can still be
// replaced between the check and the action. On a socket admitted on an
// expected birth the action itself runs bound or carries the origin
// (actionContext, actionExpectation); on any other socket it keeps the
// unbound authCtx. A resume or a child run never inherits the binding.
func (c *runConn) originHolds(env runWSEnvelope) bool {
	if c.xStore != nil || !c.origin.Observed() {
		return true
	}
	bound, err := c.streamCtx()
	if err == nil {
		ctx, cancel := context.WithTimeout(bound, runWSStandingTimeout)
		_, err = c.server.runs.LoadRunCtx(ctx, c.runID)
		cancel()
	}
	if err == nil {
		return true
	}
	if runOriginLost(err) {
		if c.server.logger != nil {
			c.server.logger.Warn("server: ws of run %s refused a %s: %v", c.runID, env.Type, err)
		}
		c.sendError(wsErrRunOriginLost, wsCauseOriginLost+" — reopen it to act on the run that holds this ID now", env.AckID)
		c.closeOnOriginLost(err)
		return false
	}
	if c.server.logger != nil {
		c.server.logger.Error("server: ws of run %s: its origin could not be checked: %v", c.runID, err)
	}
	c.sendError("scope_unavailable", "the run this socket watches could not be checked — retry", env.AckID)
	return false
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
	if err := c.requireBoundSource(); err != nil {
		c.sendError("event_stream_failed", err.Error(), env.AckID)
		return
	}
	parent, err := c.streamCtx()
	if err != nil {
		c.sendError("snapshot_failed", err.Error(), env.AckID)
		return
	}
	owner, created := c.reserveSubscription(parent, false)
	if !created {
		if owner != nil {
			c.sendAck(env.AckID)
		}
		return
	}
	var sub runstream.EventSubscription
	handedOff := false
	defer func() {
		if !handedOff {
			if sub != nil {
				_ = sub.Close()
			}
			c.clearFailedSubscription(owner, false)
			c.finishSubscription(owner)
		}
	}()
	snap, err := c.snapshotCtx(owner.ctx)
	if err != nil {
		if owner.ctx.Err() == nil {
			c.sendError("snapshot_failed", err.Error(), env.AckID)
			c.closeOnOriginLost(err)
		}
		return
	}
	if owner.ctx.Err() != nil {
		return
	}
	if c.identity.Role != "" {
		snap.Viewer = &runview.RunViewer{Role: string(c.identity.Role), Via: c.identity.Via}
	}
	if !c.sendEnvelope(wsTypeSnapshot, snap, env.AckID) {
		return
	}
	position := runstream.EventCursor{Seq: req.FromSeq}
	if !req.ReplayHistory && snap.LastSeq != runview.NoEventsSeq {
		position = runstream.EventCursor{Seq: snap.LastSeq, After: true}
	}
	sub, err = runstream.SubscribeEventsCursor(owner.ctx, c.src, c.runID, position)
	if err != nil {
		if owner.ctx.Err() == nil {
			c.sendError("event_stream_failed", err.Error(), "")
			c.closeOnOriginLost(err)
		}
		return
	}
	if sub == nil {
		c.sendError("event_stream_failed", "source returned no subscription", "")
		return
	}
	if !c.ownsSubscription(owner, false) {
		return
	}
	handedOff = true
	errtrack.Go("server.runWS.pumpEvents", func() { defer c.finishSubscription(owner); c.pumpEventsCtx(owner.ctx, sub) })
}

// snapshot builds the subscribe-time snapshot from whichever store this
// connection observes — the only remaining storage difference in the
// WS layer (the foreign store has no Service wrapper to ask).
func (c *runConn) snapshotCtx(ctx context.Context) (*runview.RunSnapshot, error) {
	if c.xStore != nil {
		return runview.BuildSnapshot(ctx, c.xStore, c.runID)
	}
	return c.server.runs.SnapshotCtx(ctx, c.runID)
}

// pumpEvents forwards the subscription to the WS: replay pages ship as
// event_batch envelopes (one frame per page instead of one per event),
// live deliveries as single event envelopes. The Events channel closing
// means the stream ended. Already-published permanent errors become error
// envelopes (preserving events_corrupted); clean completion becomes terminated.
// Transient source errors (e.g. a
// change-stream reconnect notice) are logged and the stream stays open.
func (c *runConn) pumpEvents(sub runstream.EventSubscription) {
	c.pumpEventsCtx(c.authCtx(), sub)
}
func (c *runConn) pumpEventsCtx(ctx context.Context, sub runstream.EventSubscription) {
	defer func() { _ = sub.Close() }()
	events := sub.Events()
	errs := sub.Errors()
	var fatal error
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case batch, ok := <-events:
			if !ok {
				fatal = drainRunStreamFatal(fatal, errs)
				if errors.Is(fatal, store.ErrEventsCorrupted) {
					c.sendError("events_corrupted", fatal.Error(), "")
				} else if fatal != nil {
					c.sendError("event_stream_failed", fatal.Error(), "")
					c.closeOnOriginLost(fatal)
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
			fatal = retainRunStreamFatal(fatal, err)
			if c.server.logger != nil {
				c.server.logger.Warn("server: ws event stream %s: %v", c.runID, err)
			}
		}
	}
}

func (c *runConn) handleUnsubscribe(env runWSEnvelope) {
	c.unsubscribe(false)
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
		c.server.logger.Info("server: cancel run %q via WS from %s", c.runID, c.clientIP)
	}
	// On a socket admitted on an expected birth the cancel is the HTTP
	// one's: the cloud flip and the inactive flip are bound to the socket's
	// origin, the ID-keyed signals behind originHolds' bound load, and a
	// refusal on a lost origin is run_origin_lost, never an ack.
	ctx, checked, cerr := c.actionContext()
	if cerr != nil {
		c.sendScopeUnavailable(env, cerr)
		return
	}
	var err error
	if checked {
		err = c.server.runs.CancelCtx(context.WithoutCancel(ctx), c.runID)
	} else {
		err = c.server.runs.Cancel(c.runID)
	}
	if err != nil && errors.Is(err, runview.ErrRunNotActive) {
		// Match the HTTP handler's behaviour: dispatcher-spawned runs
		// aren't tracked by the runview Manager — try the dispatcher's
		// own cancel-by-runID path first, then fall through to flipping
		// a paused / failed_resumable run to cancelled.
		if c.server.cfg.Dispatcher != nil && c.server.cfg.Dispatcher.CancelRun(c.runID) {
			c.sendAck(env.AckID)
			return
		}
		if _, ciErr := c.server.runs.CancelInactiveCtx(ctx, c.runID); ciErr != nil {
			if checked && c.writeActionFailure(env, ciErr) {
				return
			}
			if c.server.logger != nil {
				c.server.logger.Warn("server: ws cancel of inactive run %s: %v", c.runID, ciErr)
			}
		}
		err = nil
	}
	if err != nil {
		if checked && c.writeActionFailure(env, err) {
			return
		}
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
		c.server.logger.Info("server: pause run %q via WS from %s", c.runID, c.clientIP)
	}
	// The pause is an ID-keyed signal: originHolds' bound load is its
	// pre-check. On a socket admitted on an expected birth a refusal on a
	// lost origin is run_origin_lost, as over HTTP.
	_, checked, cerr := c.actionContext()
	if cerr != nil {
		c.sendScopeUnavailable(env, cerr)
		return
	}
	if err := c.server.runs.Pause(c.runID); err != nil {
		if checked && c.writeActionFailure(env, err) {
			return
		}
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
	//
	// The subject comes from the run this socket is already attached to (no
	// new exposure: the caller reached it to open the stream). It matters in
	// the direction nobody expects — judged as ordinary work, a RESERVED bot
	// faces the ceiling its OWN reservation lowered, so answering a paused
	// review-pr could be refused on the band held for it, and where the
	// reserves take a whole cap no question could be answered at all.
	// resumeLaunchSubject is the seam the HTTP resume and the retry sweeper
	// cross too: the run's TRUST and, on a fork run, the author subject the
	// launch metered ride it, so an untrusted answer re-enters the same bound
	// the launch entered.
	//
	// ONE read serves the gate's subject and the resume below, as in
	// handleResumeRun: a run the store cannot answer for is gated on the
	// ordinary subject and its failure answered before anything resumes —
	// never a later read the gate did not judge. What the gate admitted
	// rides the ResumeSpec, and the run service refuses a resume whose
	// document names another trust.
	//
	// On a socket admitted on an expected birth the resume carries the
	// socket's origin (ResumeSpec.ExpectOrigin): the run service verifies the
	// run it re-enters, so a successor recreated under the ID is refused,
	// and every failure on a lost origin is run_origin_lost. That load and
	// the chat-history read stay unbound, as the HTTP resume's do.
	expect, eerr := c.actionExpectation()
	if eerr != nil {
		c.sendScopeUnavailable(env, eerr)
		return
	}
	var (
		runMeta *store.Run
		loadErr = errors.New("run service unavailable")
		subj    launchSubject
	)
	if c.server.runs != nil {
		runMeta, loadErr = c.server.runs.LoadRunCtx(c.authCtx(), c.runID)
		if loadErr == nil && runMeta != nil {
			subj = c.server.resumeLaunchSubject(runMeta)
		}
	}
	adm, d := c.server.gateLaunch(auth.WithIdentity(c.authCtx(), c.identity), subj)
	if d != nil {
		// The subject was read from the run behind the ID now: on a lost
		// origin the denial judged another run, and the loss is the answer.
		c.refuseChecked(env, expect.Checked(), d.err(), d.reason, d.detail)
		return
	}
	// The gate's run-quota increment IS the metering, so the returns between
	// here and the resume below abandon an admitted launch and hand the unit
	// back (a bad payload, an empty answer set, a run this connection cannot
	// load). Past the resume the slot is spent unless the run service reports
	// that nothing started — a publish can report failure after the runner
	// claimed the message, and that case keeps its unit. Same rule as
	// handleLaunchRun / handleResumeRun.
	runMayExist := false
	defer func() {
		if !runMayExist {
			adm.rollback(c.server.logger)
		}
	}()
	var req wsAnswerRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		c.sendError("bad_payload", err.Error(), env.AckID)
		return
	}
	if len(req.Answers) == 0 {
		c.sendError("no_answers", "answers is required", env.AckID)
		return
	}
	if loadErr != nil || runMeta == nil {
		if expect.Checked() && c.writeActionFailure(env, loadErr) {
			return
		}
		msg := "run not found"
		if loadErr != nil {
			msg = loadErr.Error()
		}
		c.sendError("run_not_found", msg, env.AckID)
		return
	}
	filePath := req.FilePath
	if filePath == "" {
		filePath = runMeta.FilePath
		if filePath == "" && req.Source == "" {
			const msg = "run has no persisted FilePath; supply file_path or source in payload"
			c.refuseChecked(env, expect.Checked(), errors.New(msg), "file_path_required", msg)
			return
		}
	}
	absPath, err := c.server.resolveWorkflowPath(filePath, req.Source)
	if err != nil {
		code := "invalid_file_path"
		if errors.Is(err, bundle.ErrAuthorDocument) {
			code = "author_document"
		}
		c.refuseChecked(env, expect.Checked(), err, code, err.Error())
		return
	}
	hostInputs, err := c.server.assistantChatHostInputs(c.authCtx(), runMeta)
	if err != nil {
		if expect.Checked() && c.writeActionFailure(env, err) {
			return
		}
		c.sendError("chat_history_failed", err.Error(), env.AckID)
		return
	}
	// Use authCtx (Background-derived, carries tenant/user identity) so
	// closing the browser tab doesn't cancel the resume but the mongo
	// tenant_id filter still applies on writes.
	runMayExist = true
	if _, err := c.server.runs.Resume(c.authCtx(), runview.ResumeSpec{
		RunID:        c.runID,
		FilePath:     absPath,
		Source:       req.Source,
		Answers:      req.Answers,
		HostInputs:   hostInputs,
		Admitted:     gateAdmitted(subj, adm),
		ExpectOrigin: expect,
	}); err != nil {
		// The callee reports whether anything durable happened — see
		// handleLaunchRun. ErrRunNotResumable carries no marker, so the
		// routine lost race below gives its unit back.
		if !runview.RunMayHaveStarted(err) {
			runMayExist = false
		}
		if expect.Checked() && c.writeActionFailure(env, err) {
			return
		}
		// A parked gate has two legitimate resumers — the operator and the
		// assistant-watch coordinator delivering an event. The loser of that
		// race did nothing wrong, so name the case instead of surfacing a
		// generic failure the client can only display.
		if errors.Is(err, runview.ErrRunNotResumable) {
			c.sendError(runNotResumableErrorCode, err.Error(), env.AckID)
			return
		}
		// A key the run sealed is full of other runs: the run was put back
		// as it was, and the same answer succeeds once a slot frees — the
		// code writeCredentialAtCapacityError answers over HTTP.
		if errors.Is(err, cloudpublisher.ErrCredentialAtCapacity) {
			c.sendError(credentialAtCapacityErrorCode, runtime.OperatorMessage(err), env.AckID)
			return
		}
		// The document the run service re-entered names another trust than
		// the one this answer was admitted on: nothing was published.
		if errors.Is(err, runview.ErrResumeNotAdmitted) {
			c.sendError(resumeNotAdmittedErrorCode, err.Error(), env.AckID)
			return
		}
		c.sendError("resume_failed", runtime.OperatorMessage(err), env.AckID)
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
	// On a socket admitted on an expected birth the load and the append are
	// bound to the socket's origin.
	ctx, checked, cerr := c.actionContext()
	if cerr != nil {
		c.sendScopeUnavailable(env, cerr)
		return
	}
	msg, err := c.server.runs.QueueMessage(ctx, c.runID, req.Text, qopts...)
	if err != nil {
		if checked && c.writeActionFailure(env, err) {
			return
		}
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
	// On a socket admitted on an expected birth the inbox CAS is bound to the
	// socket's origin.
	ctx, checked, cerr := c.actionContext()
	if cerr != nil {
		c.sendScopeUnavailable(env, cerr)
		return
	}
	if err := c.server.runs.CancelQueuedMessage(ctx, c.runID, req.MessageID); err != nil {
		if checked && c.writeActionFailure(env, err) {
			return
		}
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
// hops. A close frame on that same channel is the last thing it writes.
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
			if len(data) == 0 {
				c.mu.Lock()
				frame := c.closeFrame
				c.mu.Unlock()
				if frame != nil {
					if err := writeWSClose(c.conn, frame.code, frame.token, frame.cause); err != nil && c.server.logger != nil {
						c.server.logger.Warn("ws: close frame for run %s: %v", c.runID, err)
					}
				}
				c.close()
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
