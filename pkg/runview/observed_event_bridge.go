package runview

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

type observedBridgePhase uint8

const (
	bridgePreparing observedBridgePhase = iota
	bridgeRunning
	bridgeRetiring
	bridgeDone
)

type observedBridgeIdentity struct {
	tenant, owner                   string
	tenantSet, ownerSet, privileged bool
}

func bridgeIdentity(ctx context.Context) observedBridgeIdentity {
	tenant, ts := store.TenantFromContext(ctx)
	owner, os := store.OwnerFromContext(ctx)
	return observedBridgeIdentity{tenant, owner, ts, os, store.IsWithoutTenantFilter(ctx)}
}

// observedEventBridges owns retained filesystem producers for Service feeds.
// open is a deterministic private-test seam, not provider admission.
type observedEventBridges struct {
	closed            bool
	stopOwnerLifetime context.CancelCauseFunc
	mu                sync.Mutex
	lifetime          context.Context
	native            *store.FilesystemRunStore
	broker            *EventBroker
	logger            *iterlog.Logger
	entries           map[store.RunObservation]*observedBridgeEntry
	open              func(context.Context) (runstream.Source, error)
}
type observedBridgeEntry struct {
	registry           *observedEventBridges
	original           store.RunObservation
	identity           observedBridgeIdentity
	phase              observedBridgePhase
	refs               int
	clients            map[*observedBridgeClient]struct{}
	scope              *ObservedEventScope
	ctx                context.Context
	cancel             context.CancelCauseFunc
	stopOwner          func() bool
	ready, done        chan struct{}
	readyClosed        bool
	readyErr, finalErr error
}
type observedBridgeClient struct {
	entry      *observedBridgeEntry
	sub        *EventSubscription
	closed     bool
	stopCaller func() bool
}

// Copies of a client lease share release state. The channel may close before
// Done; buffered original events remain readable. Err is nil before terminal
// publication and stable after Done; it is always synchronized.
type observedBridgeLease struct{ client *observedBridgeClient }

func newObservedEventBridges(lifetime context.Context, owner *Service) (*observedEventBridges, error) {
	if lifetime == nil || owner == nil || owner.broker == nil || owner.streamSrc != nil {
		return nil, store.ErrRunDataCapability
	}
	if err := lifetime.Err(); err != nil {
		return nil, err
	}
	native, ok := owner.store.(*store.FilesystemRunStore)
	if !ok || native == nil {
		return nil, store.ErrRunDataCapability
	}
	r := &observedEventBridges{lifetime: lifetime, native: native, broker: owner.broker, logger: owner.logger, entries: make(map[store.RunObservation]*observedBridgeEntry)}
	r.open = func(context.Context) (runstream.Source, error) {
		return runstream.NewFileSource(native, "", r.logger), nil
	}
	return r, nil
}
func (r *observedEventBridges) admit(ctx context.Context, o store.RunObservation) (context.Context, error) {
	if r == nil || r.native == nil || ctx == nil || !o.Observed() || !o.ParentPresent() {
		return nil, store.ErrRunDataCapability
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.lifetime.Err(); err != nil {
		return nil, err
	}
	binding, err := store.CaptureRunDataBinding(o.Header())
	if err != nil {
		return nil, err
	}
	// WithRunDataBinding compares any existing binding AND explicit format before
	// adding missing read intent. Caller transport identity remains unchanged.
	bound, err := store.WithRunDataBinding(ctx, binding)
	if err != nil {
		return nil, err
	}
	return store.WithRunValueFormat(bound, store.RunValueFormat{RunID: o.Header().RunID, Format: o.Header().Format})
}
func (r *observedEventBridges) acquire(ctx context.Context, o store.RunObservation) (*observedBridgeLease, error) {
	for {
		admitted, err := r.admit(ctx, o)
		if err != nil {
			return nil, err
		}
		identity := bridgeIdentity(admitted)
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, store.ErrRunDataCapability
		}
		if err = ctx.Err(); err == nil {
			err = r.lifetime.Err()
		}
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		entry := r.entries[o]
		if entry != nil && entry.phase >= bridgeRetiring {
			done := entry.done
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-r.lifetime.Done():
				return nil, r.lifetime.Err()
			}
		}
		if entry != nil && entry.identity != identity {
			r.mu.Unlock()
			return nil, store.ErrRunDataIdentityChanged
		}
		created := entry == nil
		if created {
			scope, e := r.broker.AcquireObserved(o)
			if e != nil {
				r.mu.Unlock()
				return nil, e
			}
			worker, cancel := context.WithCancelCause(context.WithoutCancel(admitted))
			entry = &observedBridgeEntry{registry: r, original: o, identity: identity, scope: scope, ctx: worker, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), clients: make(map[*observedBridgeClient]struct{})}
			entry.stopOwner = context.AfterFunc(r.lifetime, func() { cancel(context.Cause(r.lifetime)) })
			r.entries[o] = entry
		}
		sub, e := entry.scope.Subscribe()
		if e != nil {
			r.mu.Unlock()
			if created {
				entry.cancel(e)
				go entry.run()
			}
			return nil, e
		}
		client := &observedBridgeClient{entry: entry, sub: sub}
		entry.clients[client] = struct{}{}
		entry.refs++
		client.stopCaller = context.AfterFunc(ctx, func() { client.release(false) })
		r.mu.Unlock()
		if created {
			go entry.run()
		}
		lease := &observedBridgeLease{client: client}
		select {
		case <-entry.ready:
		case <-ctx.Done():
			lease.Close()
			return nil, ctx.Err()
		case <-r.lifetime.Done():
			lease.Close()
			return nil, r.lifetime.Err()
		}
		r.mu.Lock()
		err = entry.readyErr
		closed := client.closed
		r.mu.Unlock()
		if e = ctx.Err(); e != nil {
			err = errors.Join(err, e)
		}
		if e = r.lifetime.Err(); e != nil {
			err = errors.Join(err, e)
		}
		if closed && err == nil {
			err = context.Canceled
		}
		if err != nil {
			lease.Close()
			return nil, err
		}
		return lease, nil
	}
}
func (c *observedBridgeClient) release(join bool) {
	e := c.entry
	r := e.registry
	r.mu.Lock()
	cancelSub := !c.closed
	if cancelSub {
		c.closed = true
		e.refs--
		if e.refs == 0 && e.phase < bridgeRetiring {
			e.phase = bridgeRetiring
		}
	}
	retiring := e.phase >= bridgeRetiring
	stop := c.stopCaller
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancelSub {
		c.sub.Cancel()
		// Keep a cancellation-in-flight in the cleanup snapshot. Another
		// broker owner can keep this topic open after our scope is released.
		r.mu.Lock()
		delete(e.clients, c)
		r.mu.Unlock()
	}
	if retiring {
		e.cancel(context.Canceled)
		if join {
			<-e.done
		}
	}
}
func (l *observedBridgeLease) Close() error {
	if l != nil && l.client != nil {
		l.client.release(true)
	}
	return nil
}
func (l *observedBridgeLease) Events() <-chan *store.Event { return l.client.sub.C }
func (l *observedBridgeLease) Drops() int                  { return l.client.sub.Drops() }
func (l *observedBridgeLease) Done() <-chan struct{}       { return l.client.entry.done }
func (l *observedBridgeLease) Err() error {
	e := l.client.entry
	r := e.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.finalErr
}
func (e *observedBridgeEntry) readyWith(err error) {
	r := e.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if !e.readyClosed {
		e.readyErr = err
		e.readyClosed = true
		if err == nil && e.phase == bridgePreparing {
			e.phase = bridgeRunning
		}
		close(e.ready)
	}
}
func nilBridgeResource(v any) bool {
	if v == nil {
		return true
	}
	x := reflect.ValueOf(v)
	switch x.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		return x.IsNil()
	}
	return false
}
func (e *observedBridgeEntry) finish(src runstream.Source, sub runstream.EventSubscription, result error) {
	r := e.registry
	r.mu.Lock()
	e.phase = bridgeRetiring
	clients := make([]*observedBridgeClient, 0, len(e.clients))
	for c := range e.clients {
		clients = append(clients, c)
	}
	r.mu.Unlock()
	// Keep the retiring table entry until all captured clients and owned source
	// work are stopped. Otherwise a same-observation H2 can share H1's topic.
	e.readyWith(result)
	for _, c := range clients {
		if c.stopCaller != nil {
			c.stopCaller()
		}
		c.sub.Cancel()
	}
	// Preserve a cause already present; do not turn normal EOF into our own cancellation.
	result = errors.Join(result, context.Cause(e.ctx))
	e.cancel(context.Canceled)
	if !nilBridgeResource(sub) {
		result = errors.Join(result, sub.Close())
	}
	if !nilBridgeResource(src) {
		result = errors.Join(result, src.Close())
	}
	// Native Close joins its producer; Errors is now producer-closed. Test
	// resources must honor that same private lifecycle contract.
	if !nilBridgeResource(sub) && sub.Errors() != nil {
		for err := range sub.Errors() {
			if runstream.IsPermanentError(err) {
				result = errors.Join(result, err)
			}
		}
	}
	if e.stopOwner != nil {
		e.stopOwner()
	}
	result = errors.Join(result, e.scope.Close())
	r.mu.Lock()
	e.finalErr = result
	e.phase = bridgeDone
	if r.entries[e.original] == e {
		delete(r.entries, e.original)
	}
	close(e.done)
	r.mu.Unlock()
}
func (e *observedBridgeEntry) run() {
	var src runstream.Source
	var sub runstream.EventSubscription
	var result error
	defer func() { e.finish(src, sub, result) }()
	if result = context.Cause(e.ctx); result != nil {
		return
	}
	src, result = e.registry.open(e.ctx)
	if result != nil {
		return
	}
	if nilBridgeResource(src) {
		result = store.ErrRunDataCapability
		return
	}
	sub, result = src.SubscribeEvents(e.ctx, e.original.Header().RunID, math.MinInt64)
	if result != nil {
		return
	}
	if nilBridgeResource(sub) {
		result = store.ErrRunDataCapability
		return
	}
	if result = context.Cause(e.ctx); result != nil {
		return
	}
	if sub.Events() == nil || sub.Errors() == nil {
		result = store.ErrRunDataCapability
		return
	}
	e.readyWith(nil)
	events, errs := sub.Events(), sub.Errors()
	for {
		select {
		case <-e.ctx.Done():
			result = context.Cause(e.ctx)
			return
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if runstream.IsPermanentError(err) {
				result = err
				return
			}
			if err != nil && e.registry.logger != nil {
				e.registry.logger.Warn("observed event bridge: %v", err)
			}
		case batch, ok := <-events:
			if !ok {
				return
			}
			for _, evt := range batch {
				if evt == nil {
					result = store.ErrRunDataCapability
					return
				}
				o, known := evt.EventObservation()
				if !known {
					result = store.ErrRunDataCapability
					return
				}
				if o != e.original || evt.RunID != e.original.Header().RunID {
					result = store.ErrRunDataIdentityChanged
					return
				}
			}
			for _, evt := range batch {
				if result = context.Cause(e.ctx); result != nil {
					return
				}
				if result = e.scope.Publish(*evt); result != nil {
					return
				}
			}
		}
	}
}

// stop closes admission before cancellation and joins owned cleanup within ctx.
// AfterFunc stop callbacks are not themselves joined; they only finish captured
// idempotent bookkeeping and cannot reserve or operate on a successor.
func (r *observedEventBridges) stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	entries := make([]*observedBridgeEntry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e)
	}
	r.mu.Unlock()
	// Cancellation is mandatory even with an already expired join budget.
	if r.stopOwnerLifetime != nil {
		r.stopOwnerLifetime(context.Canceled)
	}
	for _, e := range entries {
		e.cancel(context.Canceled)
	}
	for _, e := range entries {
		select {
		case <-e.done:
			continue
		default:
		}
		select {
		case <-e.done:
		case <-ctx.Done():
			// Prefer a completed join if cancellation raced its final publication.
			select {
			case <-e.done:
				continue
			default:
				return ctx.Err()
			}
		}
	}
	return nil
}
