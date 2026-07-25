package runtime

import "sync"

// answersDoorbell is the in-process fast-path waking await_answers nodes
// (ADR-081) when an async interaction is answered. Closed-and-replaced
// channel, same discipline as the runEvents registry: ring() closes the
// current channel (waking every parked waiter) and installs a fresh one.
//
// It is a FAST PATH only — correctness never depends on it. A cross-process
// answer (CLI writing the store while the engine runs elsewhere) misses the
// bell and is picked up by the await node's periodic store re-check.
type answersDoorbell struct {
	mu sync.Mutex
	ch chan struct{}
}

// wait returns the channel to select on; it is closed at the next ring.
func (d *answersDoorbell) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	return d.ch
}

// ring wakes every current waiter.
func (d *answersDoorbell) ring() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch != nil {
		close(d.ch)
	}
	d.ch = make(chan struct{})
}

// NotifyInteractionAnswered wakes any await_answers node parked in this
// engine's run so it re-checks the interaction store immediately instead of
// waiting for its next poll tick. Safe to call from any goroutine, at any
// point of the run lifecycle, including when nothing is waiting.
func (e *Engine) NotifyInteractionAnswered() {
	e.answersBell.ring()
}
