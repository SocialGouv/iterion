package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync/atomic"
	"testing"
	"time"
)

type ownedHeldSub struct {
	entered, release chan struct{}
	events           chan []*store.Event
	chunks           chan runstream.LogChunk
	closes           atomic.Int32
	onClose          func()
}

func (s *ownedHeldSub) Events() <-chan []*store.Event { close(s.entered); <-s.release; return s.events }
func (s *ownedHeldSub) Chunks() <-chan runstream.LogChunk {
	close(s.entered)
	<-s.release
	return s.chunks
}
func (s *ownedHeldSub) Errors() <-chan error { return nil }
func (s *ownedHeldSub) Close() error {
	s.closes.Add(1)
	if s.onClose != nil {
		s.onClose()
	}
	return nil
}
func waitOwnerSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not finish")
	}
}
func TestWSOwnerUnsubscribeJoinsBothPumps(t *testing.T) {
	t.Parallel()
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprint(logs), func(t *testing.T) {
			c := newRunConn(&Server{}, nil, "r")
			owner, created := c.reserveSubscription(context.Background(), logs)
			if !created {
				t.Fatal("not reserved")
			}
			sub := &ownedHeldSub{entered: make(chan struct{}), release: make(chan struct{}), events: make(chan []*store.Event), chunks: make(chan runstream.LogChunk)}
			sub.onClose = func() { probeWSOwnerConnectionLock(c) }
			go func() {
				defer c.finishSubscription(owner)
				if logs {
					c.pumpLogsCtx(owner.ctx, sub)
				} else {
					c.pumpEventsCtx(owner.ctx, sub)
				}
			}()
			waitOwnerSignal(t, sub.entered)
			unsubDone := make(chan struct{})
			go func() {
				defer close(unsubDone)
				if logs {
					c.handleUnsubscribeLogs(runWSEnvelope{AckID: "u"})
				} else {
					c.handleUnsubscribe(runWSEnvelope{AckID: "u"})
				}
			}()
			// Cancellation is an explicit handshake, not a sleep deciding if unsubscribe ran.
			waitOwnerSignal(t, owner.ctx.Done())
			select {
			case <-unsubDone:
				t.Error("unsubscribe returned before held pump finished")
			default:
			}
			select {
			case raw := <-c.sendCh:
				t.Errorf("frame before pump joined: %s", raw)
			default:
			}
			close(sub.release)
			waitOwnerSignal(t, unsubDone)
			if sub.closes.Load() != 1 {
				t.Fatal("source Close must have one execution owner")
			}
			var ack runWSEnvelope
			if err := json.Unmarshal(readOwnerFrame(t, c), &ack); err != nil {
				t.Fatal(err)
			}
			if ack.AckID != "u" || ack.Type != wsTypeAck {
				t.Fatalf("not unsubscribe ACK: %+v", ack)
			}
			select {
			case raw := <-c.sendCh:
				t.Fatalf("stale frame after ACK: %s", raw)
			default:
			}
			next, created := c.reserveSubscription(context.Background(), logs)
			if !created {
				t.Fatal("unsubscribe did not permit replacement")
			}
			c.finishSubscription(next)
			c.unsubscribe(logs)
		})
	}
}

type ownerAdmissionSource struct {
	entered, release chan struct{}
	sub              *ownedHeldSub
	sourceCloses     atomic.Int32
}

func (s *ownerAdmissionSource) SubscribeEvents(context.Context, string, int64) (runstream.EventSubscription, error) {
	return nil, fmt.Errorf("events outside this fixture")
}
func (s *ownerAdmissionSource) SubscribeLogs(context.Context, string, int64) (runstream.LogSubscription, error) {
	close(s.entered)
	<-s.release
	return s.sub, nil
}
func (s *ownerAdmissionSource) Close() error { s.sourceCloses.Add(1); return nil }
func TestWSOwnerLateAdmissionClosesOutsideConnectionOnce(t *testing.T) {
	t.Parallel()
	c := newRunConn(&Server{}, nil, "r")
	sub := &ownedHeldSub{}
	sub.onClose = func() { probeWSOwnerConnectionLock(c); c.close() }
	src := &ownerAdmissionSource{entered: make(chan struct{}), release: make(chan struct{}), sub: sub}
	c.src = src
	handlerDone := make(chan struct{})
	go func() { defer close(handlerDone); c.handleSubscribeLogs(runWSEnvelope{AckID: "s"}) }()
	waitOwnerSignal(t, src.entered)
	closeDone := make(chan struct{})
	go func() { defer close(closeDone); c.close() }()
	waitOwnerSignal(t, closeDone)
	if src.sourceCloses.Load() != 0 {
		t.Fatal("close invoked a potentially reentrant source callback")
	}
	close(src.release)
	waitOwnerSignal(t, handlerDone)
	c.finishConnection()
	if sub.closes.Load() != 1 {
		t.Fatalf("late sub close count=%d", sub.closes.Load())
	}
	if src.sourceCloses.Load() != 0 {
		t.Fatal("connection closed shared source")
	}
	select {
	case raw := <-c.sendCh:
		t.Fatalf("late admission produced a frame: %s", raw)
	default:
	}
}
func TestWSOwnerNaturalCompletionKeepsDuplicateSubscribe(t *testing.T) {
	t.Parallel()
	c := newRunConn(&Server{}, nil, "r")
	owner, created := c.reserveSubscription(context.Background(), true)
	if !created {
		t.Fatal("not created")
	}
	c.finishSubscription(owner)
	c.handleSubscribeLogs(runWSEnvelope{AckID: "duplicate"})
	var env runWSEnvelope
	if err := json.Unmarshal(readOwnerFrame(t, c), &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != wsTypeAck || env.AckID != "duplicate" {
		t.Fatalf("duplicate: %+v", env)
	}
	c.unsubscribe(true)
}

func readOwnerFrame(t *testing.T, c *runConn) []byte {
	t.Helper()
	select {
	case raw := <-c.sendCh:
		return raw
	case <-time.After(2 * time.Second):
		t.Fatal("missing owner frame")
		return nil
	}
}

// Acquiring the connection mutex is the oracle: Close must not invoke a
// reentrant source callback while it holds that mutex.
func probeWSOwnerConnectionLock(c *runConn) {
	c.mu.Lock()
	c.mu.Unlock() //nolint:staticcheck // SA2001: intentionally probe reentrant acquisition without protected work.
}
