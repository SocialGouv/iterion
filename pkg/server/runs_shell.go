package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/runshell"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Post-mortem run shell: GET /api/ws/runs/{id}/shell upgrades to a
// WebSocket bridging an interactive $SHELL spawned in the run's
// PRESERVED worktree. Local-mode only, terminal runs only.
//
// Wire protocol (deliberately NOT the runs_ws envelope): PTY bytes are
// 8-bit binary streams, so both directions use BinaryMessage frames
// verbatim — no base64 envelope tax on `less`/ANSI output. The few
// control signals ride small TextMessage JSON frames:
//
//	C→S text: {"type":"resize","cols":N,"rows":N} | {"type":"ping"}
//	S→C text: {"type":"pong"} | {"type":"exit"} | {"type":"error","message":...}
//
// Lifecycle: one shell per connection (the worktree is the state, the
// shell is a viewer; reconnect = fresh shell). Both-sides idle for
// shellIdleTimeout — PTY OUTPUT counts as activity, so a watched
// `htop` lives — or shellMaxLifetime overall tears the session down,
// so orphaned shells never accumulate. Every input frame is a command in the
// run's worktree, so the credential AND the scope the shell was opened with
// are asked again while it is open (shellConn.standing, on the watchdog's
// tick).

const (
	defaultShellIdleTimeout = 30 * time.Minute
	defaultShellMaxLifetime = 2 * time.Hour
	// defaultShellWatchPeriod is the watchdog's tick: the idle poll, and the
	// one standing read an open shell spends per period per connection.
	defaultShellWatchPeriod = 30 * time.Second
	// shellStandingTimeout bounds that read.
	shellStandingTimeout = 10 * time.Second
)

func shellIdleTimeout() time.Duration {
	return envDurationOrDefault("ITERION_RUN_SHELL_IDLE_TIMEOUT", defaultShellIdleTimeout)
}

func shellMaxLifetime() time.Duration {
	return envDurationOrDefault("ITERION_RUN_SHELL_MAX_LIFETIME", defaultShellMaxLifetime)
}

func shellWatchPeriod() time.Duration {
	return envDurationOrDefault("ITERION_RUN_SHELL_WATCH_PERIOD", defaultShellWatchPeriod)
}

func envDurationOrDefault(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// shellEligible are the statuses whose run is at rest with a worktree
// worth inspecting. Live runs are excluded: a concurrent interactive
// writer in the worktree would race the engine's own git operations.
func shellEligible(status store.RunStatus) bool {
	switch status {
	case store.RunStatusFailed, store.RunStatusFailedResumable,
		store.RunStatusCancelled, store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator, store.RunStatusFinished:
		return true
	default:
		return false
	}
}

func (s *Server) handleRunShell(w http.ResponseWriter, r *http.Request) {
	// Gate order matters: everything answers PLAIN HTTP before the
	// upgrade so the client gets a real status code, not a broken WS.
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "the post-mortem shell is a local-mode operation")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// A shell in a FOREIGN daemon's worktree would break the "only the
	// owning daemon mutates" invariant — same rule as every write.
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrRunDeleted) {
			s.httpErrorFor(w, r, http.StatusGone, "run was deleted")
			return
		}
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if !shellEligible(run.Status) {
		s.httpErrorFor(w, r, http.StatusConflict, "run is %s — the shell opens on runs at rest (a live run's worktree belongs to the engine)", run.Status)
		return
	}
	if run.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusConflict, "run has no recorded working directory")
		return
	}
	if !dirExists(run.WorkDir) {
		s.httpErrorFor(w, r, http.StatusGone, "the run's worktree is no longer on disk (finalized or pruned)")
		return
	}

	cols, rows := shellSizeFromQuery(r)
	sess, err := runshell.Spawn(runshell.SpawnOptions{
		WorkDir: run.WorkDir,
		Env:     []string{"ITERION_RUN_ID=" + run.ID},
		Cols:    cols,
		Rows:    rows,
	})
	if err != nil {
		if errors.Is(err, runshell.ErrUnsupported) {
			s.httpErrorFor(w, r, http.StatusNotImplemented, "%v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "spawn shell: %v", err)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		sess.Terminate()
		return
	}
	if s.logger != nil {
		s.logger.Info("server: post-mortem shell opened for run %s in %s (from %s)", run.ID, run.WorkDir, r.RemoteAddr)
	}
	// Snapshot the authenticated identity: r.Context() dies with the upgrade,
	// and the credential it names is what the watchdog asks about for as long
	// as the shell is open. Zero value when no auth context reached the
	// handler (a server built without the middleware).
	opener, _ := auth.FromContext(r.Context())
	newShellConn(s, conn, sess, run.ID, opener).run()
}

func shellSizeFromQuery(r *http.Request) (cols, rows uint16) {
	parse := func(key string) uint16 {
		if n, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && n > 0 && n < 1000 {
			return uint16(n)
		}
		return 0
	}
	return parse("cols"), parse("rows")
}

type shellControlMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	// Message carries the cause of an "error" frame.
	Message string `json:"message,omitempty"`
}

type shellConn struct {
	server *Server
	conn   *websocket.Conn
	sess   *runshell.Session
	runID  string

	// identity is the credential this shell was opened with, snapshotted at
	// the upgrade. Zero value when no auth context reached the handler.
	identity auth.Identity

	// lastActivity is the freshest instant EITHER side moved bytes.
	lastActivity atomic.Int64

	writeMu   sync.Mutex // gorilla allows one concurrent writer only
	closeOnce sync.Once
}

func newShellConn(s *Server, conn *websocket.Conn, sess *runshell.Session, runID string, opener auth.Identity) *shellConn {
	c := &shellConn{server: s, conn: conn, sess: sess, runID: runID, identity: opener}
	c.touch()
	return c
}

func (c *shellConn) touch() { c.lastActivity.Store(time.Now().UnixNano()) }

func (c *shellConn) run() {
	done := make(chan struct{})
	errtrack.Go("server.shell.pumpPTYToWS", func() { c.pumpPTYToWS(done) })
	errtrack.Go("server.shell.watchIdle", func() { c.watchIdle(done) })
	c.pumpWSToPTY() // blocks until the socket dies
	close(done)
	c.close("")
}

// pumpPTYToWS streams shell output to the browser as binary frames.
// Output counts as activity (a long build's logs keep the session
// alive without keystrokes).
func (c *shellConn) pumpPTYToWS(done <-chan struct{}) {
	buf := make([]byte, 32*1024)
	for {
		n, err := c.sess.PTY.Read(buf)
		if n > 0 {
			c.touch()
			c.writeMu.Lock()
			werr := c.conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			c.writeMu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			// Shell exited (or PTY closed): tell the client, then close.
			c.writeControl(shellControlMsg{Type: "exit"})
			c.close("shell exited")
			return
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

// pumpWSToPTY feeds keystrokes (binary) and control frames (text).
func (c *shellConn) pumpWSToPTY() {
	for {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.touch()
		switch kind {
		case websocket.BinaryMessage:
			if _, err := c.sess.PTY.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var msg shellControlMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					_ = c.sess.Resize(msg.Cols, msg.Rows)
				}
			case "ping":
				c.writeControl(shellControlMsg{Type: "pong"})
			}
		}
	}
}

// watchIdle enforces the both-sides idle timeout and the absolute
// lifetime cap, and re-asks the standing of the credential the shell was
// opened with once per tick.
func (c *shellConn) watchIdle(done <-chan struct{}) {
	idle := shellIdleTimeout()
	deadline := time.NewTimer(shellMaxLifetime())
	defer deadline.Stop()
	tick := time.NewTicker(shellWatchPeriod())
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-deadline.C:
			c.writeControl(shellControlMsg{Type: "error", Message: wsCloseLifetimeReached})
			_ = c.writeClose(websocket.CloseNormalClosure, wsCloseLifetimeReached, "reopen the shell")
			c.close("shell lifetime cap reached")
			return
		case <-tick.C:
			if !c.standing() {
				return
			}
			last := time.Unix(0, c.lastActivity.Load())
			if time.Since(last) > idle {
				c.close("shell idle timeout")
				return
			}
		}
	}
}

// standing asks whether the credential this shell was opened with still
// stands AND the scope its upgrade resolved still holds — the run's team and
// the caller's member standing in it, the same computation every open
// command surface runs on its tick (runSocketStanding) — at most one store
// read per shellWatchPeriod() per connection. Every input frame is a command
// in the run's worktree, so the questions the upgrade asked are owed again
// for as long as the shell is open: a refusal or a lost scope ends the
// session, naming the bound in an error frame and in the close frame. A read
// that did not complete leaves it open — the server's own failure is no
// verdict on the credential or the scope.
//
// A shell carrying no credential is one no auth context ever reached (a
// server built without the middleware); dev mode stands by construction.
func (c *shellConn) standing() bool {
	if c.identity.Credential == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellStandingTimeout)
	defer cancel()
	verdict := c.server.runSocketStanding(ctx, c.runID, c.identity)
	switch verdict.outcome {
	case socketStands, socketUnreadable:
		return true
	case socketCredentialRefused:
		c.writeControl(shellControlMsg{Type: "error", Message: wsCloseCredentialRefused + ": " + verdict.cause})
		_ = c.writeClose(websocket.ClosePolicyViolation, wsCloseCredentialRefused, verdict.cause)
		c.close("the credential it was opened with no longer stands: " + verdict.cause)
		return false
	case socketScopeLost:
		c.writeControl(shellControlMsg{Type: "error", Message: wsCloseScopeLost + ": " + verdict.cause})
		_ = c.writeClose(websocket.ClosePolicyViolation, wsCloseScopeLost, verdict.cause)
		c.close("the scope its upgrade resolved no longer holds: " + verdict.cause)
		return false
	}
	return true
}

// writeClose sends the close frame under this connection's write lock, the
// same discipline every other write here follows.
func (c *shellConn) writeClose(code int, token, cause string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeWSClose(c.conn, code, token, cause)
}

func (c *shellConn) writeControl(msg shellControlMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
}

func (c *shellConn) close(reason string) {
	c.closeOnce.Do(func() {
		if reason != "" && c.server.logger != nil {
			c.server.logger.Info("server: post-mortem shell for run %s closed: %s", c.runID, reason)
		}
		c.sess.Terminate()
		_ = c.conn.Close()
	})
}
