package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type wsOwnerBoundarySub struct {
	entered, release chan struct{}
	events           chan []*store.Event
	logs             chan runstream.LogChunk
	closes           atomic.Int32
	closeFn          func()
}

func (s *wsOwnerBoundarySub) Events() <-chan []*store.Event {
	if s.entered != nil {
		close(s.entered)
		<-s.release
	}
	return s.events
}
func (s *wsOwnerBoundarySub) Chunks() <-chan runstream.LogChunk {
	if s.entered != nil {
		close(s.entered)
		<-s.release
	}
	return s.logs
}
func (s *wsOwnerBoundarySub) Errors() <-chan error { return nil }
func (s *wsOwnerBoundarySub) Close() error {
	s.closes.Add(1)
	if s.closeFn != nil {
		s.closeFn()
	}
	return nil
}

type wsOwnerBoundarySource struct {
	subs     []*wsOwnerBoundarySub
	calls    atomic.Int32
	contexts chan context.Context
	closes   atomic.Int32
}

func (s *wsOwnerBoundarySource) next(ctx context.Context) *wsOwnerBoundarySub {
	i := s.calls.Add(1) - 1
	s.contexts <- ctx
	return s.subs[i]
}
func (s *wsOwnerBoundarySource) SubscribeEvents(ctx context.Context, _ string, _ int64) (runstream.EventSubscription, error) {
	return s.next(ctx), nil
}
func (s *wsOwnerBoundarySource) SubscribeLogs(ctx context.Context, _ string, _ int64) (runstream.LogSubscription, error) {
	return s.next(ctx), nil
}
func (s *wsOwnerBoundarySource) Close() error { s.closes.Add(1); return nil }
func wsOwnerBoundaryWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("ownership handshake timed out")
	}
}
func TestWSOwnerWireResubscribeWaitsForCleanup(t *testing.T) {
	t.Parallel()
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprint(logs), func(t *testing.T) {
			srv, _ := newTestServer(t)
			seedRun(t, srv, "r", "wf", store.RunStatusFinished)
			first := &wsOwnerBoundarySub{entered: make(chan struct{}), release: make(chan struct{}), events: make(chan []*store.Event), logs: make(chan runstream.LogChunk)}
			close(first.events)
			close(first.logs)
			var releaseOnce sync.Once
			releaseFirst := func() { releaseOnce.Do(func() { close(first.release) }) }
			next := &wsOwnerBoundarySub{events: make(chan []*store.Event), logs: make(chan runstream.LogChunk)}
			src := &wsOwnerBoundarySource{subs: []*wsOwnerBoundarySub{first, next}, contexts: make(chan context.Context, 2)}
			done := make(chan struct{})
			rcch := make(chan *runConn, 1)
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if e != nil {
					return
				}
				c := newRunConn(srv, ws, "r")
				c.src = src
				rcch <- c
				c.run()
				close(done)
			}))
			defer hs.Close()
			defer releaseFirst()
			ws, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http"), nil)
			if e != nil {
				t.Fatal(e)
			}
			defer ws.Close()
			rc := <-rcch
			first.closeFn = func() {
				probeWSOwnerConnectionLock(rc)
				if first.closes.Load() != 1 {
					panic("double close")
				}
			}
			subType, unsubType, firstFrame := wsTypeSubscribe, wsTypeUnsubscribe, wsTypeSnapshot
			if logs {
				subType, unsubType, firstFrame = wsTypeSubscribeLogs, wsTypeUnsubscribeLog, wsTypeAck
			}
			writeJSONMessage(t, ws, runWSEnvelope{Type: subType, AckID: "s"})
			readEnvelope(t, ws, firstFrame)
			ctx := <-src.contexts
			wsOwnerBoundaryWait(t, first.entered)
			writeJSONMessage(t, ws, runWSEnvelope{Type: unsubType, AckID: "u"})
			wsOwnerBoundaryWait(t, ctx.Done())
			frames := make(chan runWSEnvelope, 1)
			readErr := make(chan error, 1)
			go func() {
				var env runWSEnvelope
				e := ws.ReadJSON(&env)
				if e != nil {
					readErr <- e
				} else {
					frames <- env
				}
			}()
			select {
			case env := <-frames:
				t.Errorf("frame before cleanup completion: %+v", env)
			case e := <-readErr:
				t.Error(e)
			case <-time.After(50 * time.Millisecond):
			}
			releaseFirst()
			select {
			case env := <-frames:
				if env.Type != wsTypeAck || env.AckID != "u" {
					t.Fatalf("old frame at unsubscribe boundary: %+v", env)
				}
			case e := <-readErr:
				t.Fatal(e)
			case <-time.After(2 * time.Second):
				t.Fatal("missing wire ACK")
			}
			if first.closes.Load() != 1 {
				t.Fatal("ACK before unique Close")
			}
			writeJSONMessage(t, ws, runWSEnvelope{Type: subType, AckID: "next"})
			env := readEnvelope(t, ws)
			if env.Type != firstFrame || env.AckID != "next" {
				t.Fatalf("stale frame after unsubscribe ACK: %+v", env)
			}
			<-src.contexts
			writeJSONMessage(t, ws, runWSEnvelope{Type: wsTypePing, AckID: "alive"})
			env = readEnvelope(t, ws)
			if env.Type != wsTypePong || env.AckID != "alive" {
				t.Fatalf("old owner produced late frame: %+v", env)
			}
			ws.Close()
			wsOwnerBoundaryWait(t, done)
			if next.closes.Load() != 1 || src.closes.Load() != 0 {
				t.Fatal("lifecycle close ownership", next.closes.Load(), src.closes.Load())
			}
		})
	}
}
func TestWSOwnerFullQueueAvoidsSelfJoin(t *testing.T) {
	t.Parallel()
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprint(logs), func(t *testing.T) {
			srv, _ := newTestServer(t)
			c := newRunConn(srv, nil, "r")
			for i := 0; i < cap(c.sendCh); i++ {
				c.sendCh <- []byte(`{}`)
			}
			owner, ok := c.reserveSubscription(context.Background(), logs)
			if !ok {
				t.Fatal("reserve")
			}
			s := &wsOwnerBoundarySub{events: make(chan []*store.Event), logs: make(chan runstream.LogChunk)}
			close(s.events)
			close(s.logs)
			s.closeFn = func() { probeWSOwnerConnectionLock(c); c.close() }
			go func() {
				defer c.finishSubscription(owner)
				if logs {
					c.pumpLogsCtx(owner.ctx, s)
				} else {
					c.pumpEventsCtx(owner.ctx, s)
				}
			}()
			wsOwnerBoundaryWait(t, owner.done)
			c.finishConnection()
			if s.closes.Load() != 1 {
				t.Fatal("Close count", s.closes.Load())
			}
		})
	}
}
func TestWSOwnerNaturalCompletionDoesNotResubscribe(t *testing.T) {
	t.Parallel()
	c := newRunConn(&Server{}, nil, "r")
	s := &wsOwnerBoundarySub{logs: make(chan runstream.LogChunk)}
	close(s.logs)
	src := &wsOwnerBoundarySource{subs: []*wsOwnerBoundarySub{s}, contexts: make(chan context.Context, 1)}
	c.src = src
	c.handleSubscribeLogs(runWSEnvelope{AckID: "first"})
	c.mu.Lock()
	owner := c.logOwner
	c.mu.Unlock()
	wsOwnerBoundaryWait(t, owner.done)
	c.handleSubscribeLogs(runWSEnvelope{AckID: "duplicate"})
	if src.calls.Load() != 1 {
		t.Fatal("duplicate subscribed source")
	}
	c.handleUnsubscribeLogs(runWSEnvelope{AckID: "u"})
	if s.closes.Load() != 1 {
		t.Fatal("duplicate cleanup")
	}
	for len(c.sendCh) > 0 {
		var env runWSEnvelope
		if e := json.Unmarshal(<-c.sendCh, &env); e != nil {
			t.Fatal(e)
		}
	}
	c.finishConnection()
}

type wsOwnerBoundaryOwnedSource struct {
	entered chan struct{}
	sub     *wsOwnerBoundarySub
	closes  atomic.Int32
	onClose func()
}

func (s *wsOwnerBoundaryOwnedSource) SubscribeEvents(context.Context, string, int64) (runstream.EventSubscription, error) {
	return nil, fmt.Errorf("outside fixture")
}
func (s *wsOwnerBoundaryOwnedSource) SubscribeLogs(ctx context.Context, _ string, _ int64) (runstream.LogSubscription, error) {
	close(s.entered)
	<-ctx.Done()
	return s.sub, nil
}
func (s *wsOwnerBoundaryOwnedSource) Close() error {
	s.closes.Add(1)
	if s.onClose != nil {
		s.onClose()
	}
	return nil
}
func TestWSOwnerConnectionSourceClosesAfterPendingAdmission(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	sub := &wsOwnerBoundarySub{}
	src := &wsOwnerBoundaryOwnedSource{entered: make(chan struct{}), sub: sub}
	done := make(chan struct{})
	rcch := make(chan *runConn, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		c := newRunConn(srv, ws, "r")
		c.src = src
		c.xStore = store.Open(t.TempDir())
		src.onClose = func() { probeWSOwnerConnectionLock(c); c.close() }
		sub.closeFn = func() { probeWSOwnerConnectionLock(c); c.close() }
		rcch <- c
		c.run()
		close(done)
	}))
	defer hs.Close()
	ws, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	c := <-rcch
	defer c.close()
	writeJSONMessage(t, ws, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "late"})
	wsOwnerBoundaryWait(t, src.entered)
	c.close() // lifecycle/lifetime cancellation while the source is still admitting
	wsOwnerBoundaryWait(t, done)
	if src.closes.Load() != 1 || sub.closes.Load() != 1 {
		t.Fatalf("owned source/sub Close=%d/%d", src.closes.Load(), sub.closes.Load())
	}
	c.mu.Lock()
	late := c.logOwner
	c.mu.Unlock()
	if late != nil {
		t.Fatal("late subscription installed")
	}
}
