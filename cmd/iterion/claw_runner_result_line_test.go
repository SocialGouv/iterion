package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// A crossing whose RESULT does not fit one NDJSON line fails loudly: the
// writer refuses at the source, naming the envelope type, the size and the
// cap; the runner exits on that refusal and repeats it on the container's
// own log, which the launcher folds into the node's error. What goes with
// the envelope is the crossing's whole account — no structured result
// crosses, so nothing books its spend, and a pause over the cap is a
// failure rather than a pause. That is the channel's line cap, not a bound
// a producer can add: the pre-pause conversation cannot be truncated
// without corrupting the resumed state (see [delegate.ClampToolResult]).
func TestClawRunner_AResultOverTheChannelLineFailsWithItsSize(t *testing.T) {
	answer := strings.Repeat("x", delegate.MaxEnvelopeLineBytes+300_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk, err := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"content": answer},
				"finish_reason": "stop",
			}},
		})
		if err != nil {
			t.Errorf("marshal the provider's chunk: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderr := &lockedBuffer{}

	var (
		mu     sync.Mutex
		runErr error
		wg     sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := runClawRunner(context.Background(), stdinR, stdoutW, stderr)
		mu.Lock()
		runErr = err
		mu.Unlock()
		_ = stdoutW.Close()
	}()

	go func() {
		taskEnv, err := delegate.NewTaskEnvelope(delegate.IOTask{
			NodeID: "copi", SessionSlot: "assistant_conversation",
			Model: "openai/gpt-test", UserPrompt: "hello",
		})
		if err != nil {
			return
		}
		_ = delegate.NewEnvelopeWriter(stdinW).Write(taskEnv)
	}()

	sawResult := make(chan bool, 1)
	go func() {
		reader := delegate.NewEnvelopeReader(stdoutR)
		for {
			env, err := reader.Read()
			if err != nil {
				sawResult <- false
				return
			}
			if env.Type == delegate.EnvelopeResult {
				sawResult <- true
				return
			}
		}
	}()

	select {
	case saw := <-sawResult:
		_ = stdinW.Close()
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		if saw {
			t.Fatalf("a result envelope over the %d-byte line cap crossed", delegate.MaxEnvelopeLineBytes)
		}
		if runErr == nil {
			t.Fatal("the runner exited 0 on a result it never managed to send")
		}
		if !errors.Is(runErr, delegate.ErrEnvelopeLineTooLong) {
			t.Fatalf("the runner's exit does not carry the channel's typed refusal: %v", runErr)
		}
		for _, want := range []string{string(delegate.EnvelopeResult), fmt.Sprint(delegate.MaxEnvelopeLineBytes)} {
			if !strings.Contains(runErr.Error(), want) {
				t.Errorf("the refusal does not name %q: %v", want, runErr)
			}
		}
		if log := stderr.String(); !strings.Contains(log, "write result envelope") ||
			!strings.Contains(log, fmt.Sprint(delegate.MaxEnvelopeLineBytes)) {
			t.Errorf("the container's own log does not say which envelope the channel refused, nor its cap: %q", log)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("the runner neither answered nor exited")
	}
}

// lockedBuffer is a stderr a test goroutine can read while the runner
// writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
