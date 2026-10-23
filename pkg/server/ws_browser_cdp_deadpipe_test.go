package server

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/store"
)

// deadPipeConn is a CDP pipe whose reads fail with a fixed error: the kind
// of diagnostic a Chromium pipe death prints, long enough to overflow what
// a close frame's reason can carry.
type deadPipeConn struct {
	net.Conn
	readErr error
}

func (c *deadPipeConn) Read([]byte) (int, error) { return 0, c.readErr }

// TestBrowserCDP_ADeadPipeClosesThroughTheOneCloseWriter: the bridge's
// pipe pump reports a dead Chromium pipe with the transport code 1011 and
// the pipe error as the reason — cut to what a control frame carries and
// kept valid UTF-8, so the close frame itself always reaches the client.
// The code travels IN the frame: a reason too long for a control frame
// makes the frame the client never receives, and a dead pane reads as an
// abnormal transport drop instead of the pipe's death.
func TestBrowserCDP_ADeadPipeClosesThroughTheOneCloseWriter(t *testing.T) {
	srv, ts, _ := standingStack(t, store.RunStatusRunning)
	cause := "chromium: read remote-debugging-pipe: connection reset by peer; the renderer process died before the target could answer, diagnostic " + strings.Repeat("x", 120)
	if len(cause) <= wsCloseReasonMax {
		t.Fatalf("fixture: the pipe error is %d bytes, want one that overflows the %d a control frame carries", len(cause), wsCloseReasonMax)
	}
	reg := mcp.NewMemoryBrowserRegistry()
	chromium, bridge := net.Pipe()
	t.Cleanup(func() { _ = chromium.Close() })
	if err := reg.Attach(mcp.BrowserSession{SessionID: "sess-1", RunID: "run-1", CDPConn: &deadPipeConn{Conn: bridge, readErr: errors.New(cause)}}); err != nil {
		t.Fatalf("attach a browser session: %v", err)
	}
	srv.browserSessions = reg
	conn := dialWithTicket(t, srv, ts, "/api/runs/run-1/browser/cdp?session=sess-1", caller("u-member-a", "tenant-A"))

	ce, _ := awaitWSClose(t, conn, 10*time.Second)
	if ce.Code != websocket.CloseInternalServerErr {
		t.Fatalf("close code = %d, want %d (1011: the pipe died)", ce.Code, websocket.CloseInternalServerErr)
	}
	if len(ce.Text) > wsCloseReasonMax {
		t.Fatalf("close reason is %d bytes, want at most %d", len(ce.Text), wsCloseReasonMax)
	}
	if !strings.HasPrefix(cause, ce.Text) {
		t.Fatalf("close reason %q is not the pipe error cut to the frame's bound (the error was %q)", ce.Text, cause)
	}
}
