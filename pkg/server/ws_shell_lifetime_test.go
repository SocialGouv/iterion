package server

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRunShell_TheShellStopsAtItsLifetimeCap: the shell's absolute lifetime
// closes it the way the run console's does — a typed error frame naming the
// bound, then the close frame whose reason names lifetime_reached — so the
// client reopens one instead of reading a transport failure. Idle keeps out
// of the way: the idle timeout is left at its 30-minute default, so only
// the lifetime can end this shell.
func TestRunShell_TheShellStopsAtItsLifetimeCap(t *testing.T) {
	t.Setenv("ITERION_RUN_SHELL_MAX_LIFETIME", "300ms")
	srv, ts, guard := standingStack(t, store.RunStatusPausedWaitingHuman)
	shellWorktree(t, guard, "run-1", t.TempDir())
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1/shell", caller("u-member-a", "tenant-A"))

	ce, texts := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d, want %d (normal: reopen)", ce.Code, websocket.CloseNormalClosure)
	}
	want := expectedCloseReason(wsCloseLifetimeReached, "reopen the shell")
	if ce.Text != want {
		t.Fatalf("close reason = %q, want exactly %q", ce.Text, want)
	}
	if !containsSubstring(texts, wsCloseLifetimeReached) {
		t.Fatalf("control frames = %v, want the typed error naming %q ahead of the close", texts, wsCloseLifetimeReached)
	}
}
