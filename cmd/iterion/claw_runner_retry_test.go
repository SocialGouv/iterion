package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// The in-container claw backend retries the provider calls made in the
// container, with the launcher's budget carried by the task. The runner's own
// env is hostile on purpose — a repository's devcontainer can set it — and
// each row sets it to the opposite budget: a runner that read it, or that
// ignored the task's budget in either direction, goes red. The provider's
// 503 "overloaded" is a transient failure, so the transient budget governs,
// never smaller than the standard one; a field left zero is the built-in
// default, never a value from the runner's env.
func TestClawRunner_RetriesWithinTheLaunchersBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		budget      delegate.IORetryPolicy
		hostileEnv  string
		wantRetries int
	}{
		{name: "fail-fast", budget: delegate.IORetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1}, hostileEnv: "5", wantRetries: 0},
		{name: "one retry", budget: delegate.IORetryPolicy{MaxAttempts: 2, MaxAttemptsTransient: 2, BackoffBase: time.Millisecond}, hostileEnv: "0", wantRetries: 1},
		{name: "transient budget", budget: delegate.IORetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 3, BackoffBase: time.Millisecond}, hostileEnv: "0", wantRetries: 2},
		{name: "standard budget only", budget: delegate.IORetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}, hostileEnv: "5", wantRetries: 0},
		{name: "transient budget above the default", budget: delegate.IORetryPolicy{MaxAttemptsTransient: 8, BackoffBase: time.Millisecond}, hostileEnv: "0", wantRetries: 7},
		{name: "zero counts are the defaults", budget: delegate.IORetryPolicy{BackoffBase: time.Millisecond}, hostileEnv: "0", wantRetries: model.DefaultMaxAttemptsTransient - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"message":"overloaded","type":"server_error"}}`)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("OPENAI_API_KEY", "sk-test")
			t.Setenv("OPENAI_BASE_URL", srv.URL)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
			t.Setenv("ITERION_NODE_MAX_RETRIES", tc.hostileEnv)
			t.Setenv("ITERION_NODE_MAX_TRANSIENT_RETRIES", tc.hostileEnv)

			budget := tc.budget
			events, res := driveRunnerToResult(t, delegate.IOTask{NodeID: "n", Model: "openai/gpt-test", UserPrompt: "hello", Retry: &budget})

			if retries := countRetries(events); retries != tc.wantRetries {
				t.Fatalf("the in-container claw loop retried a 503 %d times under the launcher's budget of %d (events %v)", retries, tc.wantRetries, events)
			}
			if !strings.Contains(res.Error, "503") {
				t.Fatalf("result error = %q, want the provider's 503 surfaced as the node's error", res.Error)
			}
			if n, want := calls.Load(), int32(tc.wantRetries+1); n != want {
				t.Errorf("the provider saw %d requests, want %d", n, want)
			}
		})
	}
}

// A launcher that predates IOTask.Retry sends no budget: the runner keeps its
// built-in defaults, never the budget its own env names. The provider fails
// once, then answers — the defaults retry once and succeed, a runner that
// read its env (fail-fast here) ends on the 503.
func TestClawRunner_KeepsItsDefaultsWithoutALaunchersBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"overloaded","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
	t.Setenv("ITERION_NODE_MAX_RETRIES", "0")
	t.Setenv("ITERION_NODE_MAX_TRANSIENT_RETRIES", "0")

	events, res := driveRunnerToResult(t, delegate.IOTask{NodeID: "n", Model: "openai/gpt-test", UserPrompt: "hello"})

	if retries := countRetries(events); retries != 1 || res.Error != "" {
		t.Fatalf("with no budget on the wire the runner retried a 503 %d times and ended on %q, want the defaults' one retry, then the answer (events %v)", retries, res.Error, events)
	}
}

func countRetries(events []string) int {
	n := 0
	for _, ev := range events {
		if ev == "llm_retry" {
			n++
		}
	}
	return n
}

// driveRunnerToResult runs one task through runClawRunner and returns the
// relayed event types in order and the terminal result.
func driveRunnerToResult(t *testing.T, task delegate.IOTask) ([]string, delegate.IOResult) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = stdoutR.Close()
	})

	go func() {
		_ = runClawRunner(context.Background(), stdinR, stdoutW, io.Discard)
		_ = stdoutW.Close()
	}()
	go func() {
		taskEnv, err := delegate.NewTaskEnvelope(task)
		if err == nil {
			_ = delegate.NewEnvelopeWriter(stdinW).Write(taskEnv)
		}
	}()

	type outcome struct {
		events []string
		res    delegate.IOResult
		err    error
	}
	out := make(chan outcome, 1)
	go func() {
		var events []string
		reader := delegate.NewEnvelopeReader(stdoutR)
		for {
			env, err := reader.Read()
			if err != nil {
				out <- outcome{events: events, err: err}
				return
			}
			switch env.Type {
			case delegate.EnvelopeEvent:
				var ed delegate.EventData
				if err := json.Unmarshal(env.Data, &ed); err != nil {
					out <- outcome{events: events, err: err}
					return
				}
				events = append(events, ed.Type)
			case delegate.EnvelopeResult:
				var res delegate.IOResult
				err := json.Unmarshal(env.Data, &res)
				out <- outcome{events: events, res: res, err: err}
				return
			}
		}
	}()

	select {
	case o := <-out:
		if o.err != nil {
			t.Fatalf("reading the runner's envelopes: %v (events so far %v)", o.err, o.events)
		}
		return o.events, o.res
	case <-time.After(2 * time.Minute):
		t.Fatal("the runner produced no result within 2 minutes")
		return nil, delegate.IOResult{}
	}
}
