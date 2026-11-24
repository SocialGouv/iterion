package scripthost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// frameServer records the frame each call and each budget refusal carried.
type frameServer struct {
	mu      sync.Mutex
	called  map[int64]CallFrame
	refused map[int64]CallFrame
	values  map[string]json.RawMessage
}

func newFrameServer() *frameServer {
	return &frameServer{called: map[int64]CallFrame{}, refused: map[int64]CallFrame{}, values: map[string]json.RawMessage{}}
}

func (s *frameServer) CallTool(ctx context.Context, name string, _ json.RawMessage) (ToolOutcome, error) {
	frame, ok := CallFrameFrom(ctx)
	if !ok {
		return ToolOutcome{}, fmt.Errorf("call %q reached the server without its frame", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called[frame.ID] = frame
	return ToolOutcome{Text: "ok"}, nil
}

func (s *frameServer) RefusedOnBudget(frame CallFrame, _ string, _ json.RawMessage, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused[frame.ID] = frame
}

func (s *frameServer) StoreState(_ context.Context, key string, value json.RawMessage) error {
	s.values[key] = value
	return nil
}

func (s *frameServer) LoadState(_ context.Context, key string) (json.RawMessage, bool, error) {
	v, ok := s.values[key]
	return v, ok, nil
}

// TestThePumpHandsEachCallItsFrameAndCountsTheChannel drives the parent's
// half of the channel with the frames a program would send — a call, a
// run-state request, a batch the budget cuts short, then a state request
// past the budget — and reads what reached the server and what the parent
// counted.
func TestThePumpHandsEachCallItsFrameAndCountsTheChannel(t *testing.T) {
	t.Parallel()
	wire := bytes.NewBufferString(
		`{"type":"call","id":1,"name":"a"}` + "\n" +
			`{"type":"state_store","id":2,"name":"k","input":1}` + "\n" +
			`{"type":"batch","calls":[{"type":"call","id":3,"name":"b"},{"type":"call","id":4,"name":"c"},{"type":"call","id":5,"name":"d"}]}` + "\n" +
			`{"type":"state_load","id":6,"name":"k"}` + "\n")
	srv := newFrameServer()
	limits := DefaultLimits()
	limits.ToolCalls = 3
	stats, err := serveToolCalls(context.Background(), wire, &bytes.Buffer{}, srv, limits)
	if err != nil {
		t.Fatal(err)
	}
	if want := (CallStats{Served: 3, BudgetRefused: 3, StateCalls: 1}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	single, first := srv.called[1], srv.called[3]
	if len(srv.called) != 2 || single.ID != 1 || single.Batch != nil ||
		first.ID != 3 || first.Batch == nil || *first.Batch != (BatchSlot{First: 3, Size: 3, Index: 0}) {
		t.Errorf("served frames = %+v (batch of 3: %+v), want call 1 unbatched and call 3 in slot 0 of the batch at 3", srv.called, first.Batch)
	}
	ids := make([]int64, 0, len(srv.refused))
	for id, f := range srv.refused {
		ids = append(ids, id)
		if f.Batch == nil || *f.Batch != (BatchSlot{First: 3, Size: 3, Index: int(id - 3)}) {
			t.Errorf("refused frame %d carries batch %+v, want slot %d of the batch at 3", id, f.Batch, id-3)
		}
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{4, 5}) {
		t.Errorf("budget refusals observed for frames %v, want 4 and 5 — the run-state refusal is counted, not observed", ids)
	}
}
