package execenv

import (
	"io"
	"sync"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

// prefixWriter leaves the exec-owned stdout copier in charge of drain/WaitDelay.
// It buffers only Readiness, never a workload output frame.
type prefixWriter struct {
	mu       sync.Mutex
	dst      io.Writer
	ready    chan struct{}
	reject   func()
	prefix   [len(protocol.Readiness)]byte
	expected string
	n        int
	matched  bool
	err      error
}

func newPrefixWriter(dst io.Writer, reject func(), in protocol.Invocation) *prefixWriter {
	return &prefixWriter{dst: dst, ready: make(chan struct{}), reject: reject, expected: protocol.InvocationReadiness(in)}
}
func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return 0, p.err
	}
	used := 0
	if !p.matched {
		used = copy(p.prefix[p.n:], b)
		p.n += used
		b = b[used:]
		if p.n < len(p.prefix) {
			return used, nil
		}
		if string(p.prefix[:]) != p.expected {
			p.err = refusal("ready", "version", nil)
			if p.reject != nil {
				p.reject()
			}
			return used, p.err
		}
		p.matched = true
		close(p.ready)
	}
	if len(b) == 0 {
		return used, nil
	}
	n, err := p.dst.Write(b)
	if n < 0 || n > len(b) {
		n = 0
		err = io.ErrShortWrite
	}
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		p.err = refusal("ready", "output", err)
		if p.reject != nil {
			p.reject()
		}
	}
	return used + n, p.err
}
func (p *prefixWriter) result() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if !p.matched {
		return refusal("ready", "missing", nil)
	}
	return nil
}
