package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

type fakeFleetRecord struct {
	mu           sync.Mutex
	registerErr  error
	registered   int
	deregistered int
	running      bool
	runs         int
	// deletedWhileRunning records a delete the heartbeat could undo with
	// its next write.
	deletedWhileRunning bool
}

func (f *fakeFleetRecord) Register(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered++
	return f.registerErr
}

func (f *fakeFleetRecord) Run(ctx context.Context) {
	f.mu.Lock()
	f.running, f.runs = true, f.runs+1
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
}

func (f *fakeFleetRecord) Deregister(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		f.deletedWhileRunning = true
	}
	f.deregistered++
	return nil
}

func (f *fakeFleetRecord) heartbeatStarted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeFleetRecord) counts() (registered, deregistered, runs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registered, f.deregistered, f.runs
}

// join → leave: the heartbeat stops BEFORE the record is deleted, and a
// second leave (the deferred one after an early leave) deletes nothing more.
func TestFleetMembership_LeaveStopsTheHeartbeatThenDeletesOnce(t *testing.T) {
	rec := &fakeFleetRecord{}
	m := newFleetMembership(rec, iterlog.Nop(), "runner")
	if err := m.join(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !rec.heartbeatStarted() {
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat never started")
		}
		time.Sleep(time.Millisecond)
	}
	m.leave()
	m.leave()
	if reg, dereg, runs := rec.counts(); reg != 1 || dereg != 1 || runs != 1 {
		t.Fatalf("registered %d, deregistered %d, heartbeats %d — want 1, 1, 1", reg, dereg, runs)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.deletedWhileRunning {
		t.Fatal("the record was deleted while the heartbeat still ran: its next write would recreate it")
	}
}

// A leave before any join writes nothing, and a join after it is refused:
// nothing would ever delete the record it writes.
func TestFleetMembership_LeaveBeforeJoin(t *testing.T) {
	rec := &fakeFleetRecord{}
	m := newFleetMembership(rec, iterlog.Nop(), "server")
	m.leave()
	if err := m.join(context.Background()); !errors.Is(err, errFleetMembershipLeft) {
		t.Fatalf("join after leave = %v, want errFleetMembershipLeft", err)
	}
	if reg, dereg, _ := rec.counts(); reg != 0 || dereg != 0 {
		t.Fatalf("registered %d, deregistered %d — want nothing written", reg, dereg)
	}
}

// A registration that failed may still have landed (a lost ack): the
// record is deleted at once rather than left for the TTL, and no heartbeat
// starts.
func TestFleetMembership_AFailedJoinDeletesWhatMayHaveLanded(t *testing.T) {
	rec := &fakeFleetRecord{registerErr: errors.New("nats: timeout")}
	m := newFleetMembership(rec, iterlog.Nop(), "runner")
	if err := m.join(context.Background()); err == nil {
		t.Fatal("a failed registration joined")
	}
	m.leave()
	if reg, dereg, runs := rec.counts(); reg != 1 || dereg != 1 || runs != 0 {
		t.Fatalf("registered %d, deregistered %d, heartbeats %d — want 1, 1, 0", reg, dereg, runs)
	}
}
