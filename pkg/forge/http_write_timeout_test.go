package forge

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

type legacyCancelTransport struct {
	done chan struct{}
	once sync.Once
}

func (t *legacyCancelTransport) RoundTrip(*http.Request) (*http.Response, error) {
	<-t.done
	return nil, context.Canceled
}
func (t *legacyCancelTransport) CancelRequest(*http.Request) { t.once.Do(func() { close(t.done) }) }
func TestForgeWriteGuardPreservesLegacyClientTimeout(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		tr := &legacyCancelTransport{done: make(chan struct{})}
		c := &http.Client{Transport: tr, Timeout: 10 * time.Millisecond}
		req, _ := http.NewRequest("POST", "http://synthetic.invalid", nil)
		completed := make(chan struct{})
		go func() {
			defer close(completed)
			if guarded {
				DoHTTP(c, req)
			} else {
				c.Do(req)
			}
		}()
		select {
		case <-completed:
			t.Logf("guarded=%t timeout completed", guarded)
		case <-time.After(500 * time.Millisecond):
			tr.CancelRequest(req)
			<-completed
			t.Errorf("guarded=%t client timeout no longer cancels injected transport", guarded)
		}
	}
}
