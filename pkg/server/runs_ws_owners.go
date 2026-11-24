package server

import "context"

// A reservation owns both synchronous admission and the forwarding pump.
// Its execution owner closes the source subscription before finishing done.
type wsSubscriptionOwner struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *runConn) reserveSubscription(parent context.Context, logs bool) (*wsSubscriptionOwner, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return nil, false
	default:
	}
	existing := c.eventOwner
	if logs {
		existing = c.logOwner
	}
	if existing != nil {
		return existing, false
	}
	ctx, cancel := context.WithCancel(parent)
	owner := &wsSubscriptionOwner{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	c.subscriptionWorkers.Add(1)
	if logs {
		c.logOwner = owner
	} else {
		c.eventOwner = owner
	}
	return owner, true
}
func (c *runConn) ownsSubscription(owner *wsSubscriptionOwner, logs bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner.ctx.Err() != nil {
		return false
	}
	if logs {
		return c.logOwner == owner
	}
	return c.eventOwner == owner
}
func (c *runConn) clearFailedSubscription(owner *wsSubscriptionOwner, logs bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if logs {
		if c.logOwner == owner {
			c.logOwner = nil
		}
	} else if c.eventOwner == owner {
		c.eventOwner = nil
	}
}
func (c *runConn) finishSubscription(owner *wsSubscriptionOwner) {
	owner.cancel()
	close(owner.done)
	c.subscriptionWorkers.Done()
}
func (c *runConn) unsubscribe(logs bool) {
	c.mu.Lock()
	owner := c.eventOwner
	if logs {
		owner = c.logOwner
		c.logOwner = nil
	} else {
		c.eventOwner = nil
	}
	c.mu.Unlock()
	if owner != nil {
		owner.cancel()
		<-owner.done
	}
}

// run owns this blocking cleanup; close can be called from a forwarding pump
// and therefore only signals cancellation. No source callback runs under mu.
func (c *runConn) finishConnection() {
	c.close()
	if c.xStore != nil && c.src != nil {
		_ = c.src.Close()
	}
	c.subscriptionWorkers.Wait()
}
