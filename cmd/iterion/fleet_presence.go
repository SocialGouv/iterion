package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fleetPresenceLeaveTimeout bounds each deregistration write.
const fleetPresenceLeaveTimeout = 5 * time.Second

// fleetRecord is the registry record a process keeps (natsq.Presence).
type fleetRecord interface {
	Register(ctx context.Context) error
	Run(ctx context.Context)
	Deregister(ctx context.Context) error
}

// errFleetMembershipLeft refuses a join after the membership was left: no
// deferred leave would delete the record it writes.
var errFleetMembershipLeft = errors.New("fleet registry membership already left")

// fleetMembership is a process's stay in the fleet registry: join writes the
// record and starts its heartbeat, leave stops the heartbeat and deletes the
// record. leave is idempotent and safe before join, so an entrypoint defers
// it once and calls it early where the process stops counting (a superseded
// generation).
//
// The heartbeat runs on its own context, not the signal context: a runner
// keeps its record (draining: true) for the whole lame-duck drain, and only
// the deferred leave ends it.
type fleetMembership struct {
	record fleetRecord
	logger *iterlog.Logger
	label  string

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	left   bool
}

func newFleetMembership(record fleetRecord, logger *iterlog.Logger, label string) *fleetMembership {
	return &fleetMembership{record: record, logger: logger, label: label}
}

// join registers the record and starts its heartbeat. A failed registration
// is returned — the caller refuses to boot rather than run unseen — after a
// best-effort delete: the write may have landed with only its ack lost, and
// the record of a process that is about to exit must not count until the
// TTL expires it.
func (m *fleetMembership) join(ctx context.Context) error {
	m.mu.Lock()
	if m.left {
		m.mu.Unlock()
		return fmt.Errorf("%s: %w", m.label, errFleetMembershipLeft)
	}
	m.mu.Unlock()
	if err := m.record.Register(ctx); err != nil {
		m.deregister()
		return fmt.Errorf("%s: %w", m.label, err)
	}
	hbCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel, m.done = cancel, done
	m.mu.Unlock()
	errtrack.Go(m.label+".fleetPresence", func() {
		defer close(done)
		m.record.Run(hbCtx)
	})
	return nil
}

// leave stops the heartbeat and deletes the record, once.
func (m *fleetMembership) leave() {
	m.mu.Lock()
	if m.left {
		m.mu.Unlock()
		return
	}
	m.left = true
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel == nil {
		return // never joined: nothing was written
	}
	cancel()
	<-done
	m.deregister()
}

func (m *fleetMembership) deregister() {
	ctx, cancel := context.WithTimeout(context.Background(), fleetPresenceLeaveTimeout)
	defer cancel()
	if err := m.record.Deregister(ctx); err != nil {
		m.logger.Warn("%s: %v — the record expires with the registry TTL", m.label, err)
	}
}
