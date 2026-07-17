package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

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
// so orphaned shells never accumulate.

const (
	defaultShellIdleTimeout = 30 * time.Minute
	defaultShellMaxLifetime = 2 * time.Hour
)

func shellIdleTimeout() time.Duration {
	return envDurationOrDefault("ITERION_RUN_SHELL_IDLE_TIMEOUT", defaultShellIdleTimeout)
}

func shellMaxLifetime() time.Duration {
	return envDurationOrDefault("ITERION_RUN_SHELL_MAX_LIFETIME", defaultShellMaxLifetime)
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
	newShellConn(s, conn, sess, run.ID).run()
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
}

type shellConn struct {
	server *Server
	conn   *websocket.Conn
	sess   *runshell.Session
	runID  string

	// lastActivity is the freshest instant EITHER side moved bytes.
	lastActivity atomic.Int64

	writeMu   sync.Mutex // gorilla allows one concurrent writer only
	closeOnce sync.Once
}

func newShellConn(s *Server, conn *websocket.Conn, sess *runshell.Session, runID string) *shellConn {
	c := &shellConn{server: s, conn: conn, sess: sess, runID: runID}
	c.touch()
	return c
}

func (c *shellConn) touch() { c.lastActivity.Store(time.Now().UnixNano()) }

func (c *shellConn) run() {
	done := make(chan struct{})
	go c.pumpPTYToWS(done)
	go c.watchIdle(done)
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
// lifetime cap.
func (c *shellConn) watchIdle(done <-chan struct{}) {
	idle := shellIdleTimeout()
	deadline := time.NewTimer(shellMaxLifetime())
	defer deadline.Stop()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-deadline.C:
			c.writeControl(shellControlMsg{Type: "error"})
			c.close("shell lifetime cap reached")
			return
		case <-tick.C:
			last := time.Unix(0, c.lastActivity.Load())
			if time.Since(last) > idle {
				c.close("shell idle timeout")
				return
			}
		}
	}
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
