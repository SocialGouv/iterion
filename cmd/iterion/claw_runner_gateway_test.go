package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// driveGatewayRunner pipes pre-task envelopes and a task into
// runClawRunner and returns whether it produced a result or died with the
// named fatal. It stops reading at the first result or error line.
func driveGatewayRunner(t *testing.T, preTask []delegate.Envelope, task delegate.IOTask) (delegate.IOResult, error) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runClawRunner(context.Background(), stdinR, stdoutW, io.Discard)
		_ = stdoutW.Close()
	}()
	go func() {
		writer := delegate.NewEnvelopeWriter(stdinW)
		for _, env := range preTask {
			if err := writer.Write(env); err != nil {
				return
			}
		}
		taskEnv, err := delegate.NewTaskEnvelope(task)
		if err == nil {
			_ = writer.Write(taskEnv)
		}
	}()

	type outcome struct {
		res delegate.IOResult
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		reader := delegate.NewEnvelopeReader(stdoutR)
		for {
			env, err := reader.Read()
			if err != nil {
				out <- outcome{err: err}
				return
			}
			switch env.Type {
			case delegate.EnvelopeResult:
				var res delegate.IOResult
				_ = json.Unmarshal(env.Data, &res)
				if res.Error != "" {
					out <- outcome{err: errors.New(res.Error)}
					return
				}
				out <- outcome{res: res}
				return
			case delegate.EnvelopeEvent:
				if strings.Contains(string(env.Data), "unexpected envelope") || strings.Contains(string(env.Data), "capability marker") {
					out <- outcome{err: errors.New(string(env.Data))}
					return
				}
			}
		}
	}()
	select {
	case o := <-out:
		return o.res, o.err
	case <-time.After(10 * time.Second):
		t.Fatal("the runner neither produced a result nor died within 10s")
		return delegate.IOResult{}, nil
	}
}

var _ = sync.Once{} // keep sync imported if drive changes shape

// The gateway capability marker is fail-closed in BOTH directions: a
// gateway task without the marker (a host predating the gateway) is
// refused naming the skew; the marker before a gateway task runs; a vendor
// task without any marker runs as before. Red when the runner-side twin of
// the envelope check is dropped.
func TestClawRunner_GatewayTaskNeedsTheCapabilityMarker(t *testing.T) {
	gatewayTask := delegate.IOTask{NodeID: "gw", Model: "openai_compatible/team/m"}
	vendorTask := delegate.IOTask{NodeID: "v", Model: "anthropic/claude-opus-5"}

	_, err := driveGatewayRunner(t, nil, gatewayTask)
	if err == nil || !strings.Contains(err.Error(), "capability marker") {
		t.Fatalf("a gateway task without the marker = %v, want the skew refusal", err)
	}

	_, err = driveGatewayRunner(t, []delegate.Envelope{delegate.NewGatewayV1Envelope()}, gatewayTask)
	if err != nil && strings.Contains(err.Error(), "capability marker") {
		t.Fatalf("marker then gateway task: %v — the runner died on its own marker", err)
	}
	// Past the marker the task executes, and THIS process has no gateway
	// env: the run failing there (ErrNotConfigured) is exactly right — the
	// skew gate is behind us.

	_, err = driveGatewayRunner(t, nil, vendorTask)
	if err != nil && strings.Contains(err.Error(), "capability marker") {
		t.Fatalf("a vendor task needs no marker: %v", err)
	}
}
