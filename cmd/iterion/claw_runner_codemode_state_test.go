package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

func TestCodemodeStateRunnerRejectsUnmatchedCapabilityBeforeModel(t *testing.T) {
	for _, tc := range []struct {
		name            string
		markers         []delegate.Envelope
		codemode, state bool
	}{
		{name: "missing announcement", codemode: true, state: true},
		{name: "missing task bit", markers: []delegate.Envelope{{Type: delegate.EnvelopeCodemodeStateV1}}, codemode: true},
		{name: "codemode off", markers: []delegate.Envelope{{Type: delegate.EnvelopeCodemodeStateV1}}, state: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := driveClawRunner(t, tc.markers, delegate.IOTask{Model: "test/test-model", Codemode: tc.codemode, CodemodeState: tc.state})
			if !strings.Contains(res.Error, "codemode state v1 capability marker and task disagree") {
				t.Fatalf("missing explicit skew refusal: %s", res.Error)
			}
		})
	}
}

func TestCodemodeStateRunnerDoesNotStartAnExpiredQueuedWrite(t *testing.T) {
	fromRunner, runnerOut := io.Pipe()
	runnerIn, toRunner := io.Pipe()
	t.Cleanup(func() { _ = fromRunner.Close(); _ = runnerOut.Close(); _ = runnerIn.Close(); _ = toRunner.Close() })
	d := newProxyDispatcher(runnerIn, runnerOut)
	d.start()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	writes := make(chan struct{}, 1)
	mux := delegate.NewMultiplexer(fromRunner, toRunner, delegate.MultiplexerHandler{
		OnToolCall: func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "", nil
		},
		OnCodemodeState: func(_ context.Context, req delegate.StateRequest) (delegate.StateReply, error) {
			if req.Op == delegate.StateStore {
				writes <- struct{}{}
			}
			return delegate.StateReply{Code: delegate.StateOK}, nil
		},
	})
	done := make(chan error, 1)
	go func() { _, err := mux.Run(ctx); done <- err }()
	tool := make(chan error, 1)
	go func() { _, err := d.callTool(ctx, "ordinary", nil); tool <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	inv, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	state := runnerCodemodeState{d}
	if err := state.StoreState(inv, "item", json.RawMessage(`42`)); !errors.Is(err, scripthost.ErrStateWriteUnconfirmed) {
		t.Fatalf("unacknowledged store: %v", err)
	}
	close(release)
	if err := <-tool; err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.LoadState(ctx, "item"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writes:
		t.Fatal("expired store reached the bank")
	default:
	}
	terminal, err := delegate.NewResultEnvelope(delegate.IOResult{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.write(terminal); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCodemodeStateRunnerRelayRoundTrip(t *testing.T) {
	fromRunner, runnerOut := io.Pipe()
	runnerIn, toRunner := io.Pipe()
	t.Cleanup(func() { _ = fromRunner.Close(); _ = runnerOut.Close(); _ = runnerIn.Close(); _ = toRunner.Close() })
	dispatcher := newProxyDispatcher(runnerIn, runnerOut)
	dispatcher.start()
	state := runnerCodemodeState{dispatcher}
	bank := map[string][]byte{}
	mux := delegate.NewMultiplexer(fromRunner, toRunner, delegate.MultiplexerHandler{
		OnToolCall: func(context.Context, string, json.RawMessage) (string, error) {
			return "", errors.New("unexpected generic tool dispatch")
		},
		OnCodemodeState: func(ctx context.Context, req delegate.StateRequest) (delegate.StateReply, error) {
			if _, ok := ctx.Deadline(); !ok {
				return delegate.StateReply{}, errors.New("state request was not deadline-bound")
			}
			switch req.Op {
			case delegate.StateStore:
				if req.Key == "quota" {
					return delegate.StateReply{Code: delegate.StateQuota}, nil
				}
				bank[req.Key] = req.Value
				return delegate.StateReply{Code: delegate.StateOK}, nil
			case delegate.StateLoad:
				v, found := bank[req.Key]
				return delegate.StateReply{Code: delegate.StateOK, Found: found, Value: v}, nil
			default:
				return delegate.StateReply{}, errors.New("unexpected operation")
			}
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := mux.Run(ctx); done <- err }()
	value := json.RawMessage(`"` + strings.Repeat("<", scripthost.MaxStateValueBytes-2) + `"`)
	if err := state.StoreState(ctx, "item", value); err != nil {
		t.Fatal(err)
	}
	if got, found, err := state.LoadState(ctx, "item"); !found || err != nil || string(got) != string(value) {
		t.Fatalf("round trip changed the exact-size JSON: found=%v bytes=%d err=%v", found, len(got), err)
	}
	if _, found, err := state.LoadState(ctx, "absent"); found || err != nil {
		t.Fatalf("absent key: found=%v err=%v", found, err)
	}
	if err := state.StoreState(ctx, "quota", json.RawMessage(`null`)); !errors.Is(err, scripthost.ErrStateQuota) {
		t.Fatalf("loss of typed quota error: %v", err)
	}
	terminal, err := delegate.NewResultEnvelope(delegate.IOResult{})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.write(terminal); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
