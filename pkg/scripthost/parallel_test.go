package scripthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type parallelTestServer struct {
	call func(context.Context, string, json.RawMessage) (ToolOutcome, error)
}

func (s parallelTestServer) ConcurrentSafe(name string) bool { return name != "exclusive" }
func (s parallelTestServer) CallTool(ctx context.Context, name string, input json.RawMessage) (ToolOutcome, error) {
	return s.call(ctx, name, input)
}

func TestParallelBatchReturnsInputOrderAndReservesBudget(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"left", "right"} {
		t.Run(first, func(t *testing.T) {
			finished := make(chan struct{})
			entered := make(chan struct{})
			unexpected := errors.New("unadmitted call reached server")
			server := parallelTestServer{call: func(ctx context.Context, name string, _ json.RawMessage) (ToolOutcome, error) {
				switch name {
				case "left", "right":
					if name == first {
						select {
						case <-entered:
						case <-ctx.Done():
							return ToolOutcome{}, ctx.Err()
						}
						close(finished)
					} else {
						close(entered)
						select {
						case <-finished:
						case <-ctx.Done():
							return ToolOutcome{}, ctx.Err()
						}
					}
					return ToolOutcome{Text: name, IsError: name == "right"}, nil
				default:
					return ToolOutcome{}, unexpected
				}
			}}
			limits := smallLimits()
			limits.ToolCalls = 2
			limits.ConcurrentToolCalls = 2
			host := toolHost(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			response, err := host.Run(ctx, `call.parallel([{name:'left'},{name:'right'},{name:'unused'}]).map(x => [x.tool,x.status,x.remaining])`, limits)
			if err != nil || response.Value != `[["left","ok",0],["right","error",0],["unused","budget",0]]` {
				t.Fatalf("batch response = %+v, err = %v", response, err)
			}
		})
	}
}

func TestParallelBatchValidatesEveryEntryBeforeDispatch(t *testing.T) {
	t.Parallel()
	server := &recordingServer{}
	response, err := toolHost(t, server).Run(context.Background(), `let rejected=false; try {call.parallel([{name:'never'},{}])} catch(e){rejected=e instanceof TypeError}; [rejected,call('after').text]`, smallLimits())
	if err != nil || response.Value != `[true,"ok"]` {
		t.Fatalf("batch response = %+v, err = %v", response, err)
	}
	if seen := server.seen(); len(seen) != 1 || seen[0].Name != "after" {
		t.Fatalf("called %+v, want only after", seen)
	}
}

func TestParallelBatchAssignsIDsAfterInputGetters(t *testing.T) {
	t.Parallel()
	server := &recordingServer{}
	response, err := toolHost(t, server).Run(context.Background(), `call.parallel([{name:'left'}, {name:'right', get input(){call('getter');return {}}}]).map(r => r.tool)`, smallLimits())
	if err != nil || response.Value != `["left","right"]` {
		t.Fatalf("batch response = %+v, err = %v", response, err)
	}
	if seen := server.seen(); len(seen) != 3 || seen[0].Name != "getter" || seen[1].Name != "left" || seen[2].Name != "right" {
		t.Fatalf("dispatch order = %+v, want getter, left, right", seen)
	}
}

func TestParallelBatchAbortCancelsAndJoinsSiblings(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	joined := make(chan struct{})
	cause := errors.New("explicit tool abort")
	srv := parallelTestServer{call: func(ctx context.Context, name string, _ json.RawMessage) (ToolOutcome, error) {
		if name == "waiting" {
			close(started)
			<-ctx.Done()
			close(joined)
			return ToolOutcome{}, ctx.Err()
		}
		select {
		case <-started:
			return ToolOutcome{}, cause
		case <-ctx.Done():
			return ToolOutcome{}, ctx.Err()
		}
	}}
	limits := smallLimits()
	limits.ConcurrentToolCalls = 2
	host := toolHost(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := host.Run(ctx, `call.parallel([{name:'failing'}, {name:'waiting'}])`, limits)
	if !errors.Is(err, cause) || !errors.Is(err, errScriptAborted) {
		t.Fatalf("batch abort = %v, want typed abort wrapping cause", err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("host returned before the sibling stopped")
	}
}

func TestParallelRepliesMatchIDsInEitherOrder(t *testing.T) {
	t.Parallel()
	for _, order := range [][]int64{{1, 2}, {2, 1}} {
		var input, output bytes.Buffer
		for _, id := range order {
			if err := json.NewEncoder(&input).Encode(Frame{Type: FrameResult, ID: id, Remaining: 8}); err != nil {
				t.Fatal(err)
			}
		}
		bridge := newToolBridge(&input, &output, Limits{ToolCalls: 10})
		batch := []Frame{{ID: 1}, {ID: 2}}
		got, err := bridge.parallelTrip(batch, []byte(`{"type":"batch"}`))
		if err != nil || len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 || bridge.remaining != 8 {
			t.Fatalf("order %v: got %v, remaining %d, err %v", order, got, bridge.remaining, err)
		}
	}
}

func TestParallelWireRejectsDuplicateIDsBeforeDispatch(t *testing.T) {
	t.Parallel()
	server := &recordingServer{}
	wire := bytes.NewBufferString(`{"type":"batch","calls":[{"type":"call","id":1,"name":"first"},{"type":"call","id":1,"name":"second"}]}` + "\n")
	served, err := serveToolCalls(context.Background(), wire, &bytes.Buffer{}, server, DefaultLimits())
	if err == nil || served != 0 || len(server.seen()) != 0 {
		t.Fatalf("invalid batch: served=%d, err=%v, calls=%v", served, err, server.seen())
	}
}

func TestParallelBatchDoesNotOverlapExclusiveTools(t *testing.T) {
	t.Parallel()
	calls := []Frame{{ID: 1, Name: "first"}, {ID: 2, Name: "exclusive"}, {ID: 3, Name: "last"}}
	var seen []recordedCall
	server := &recordingServer{answer: func(name string, _ json.RawMessage) (ToolOutcome, error) { return ToolOutcome{Text: name}, nil }}
	srv := parallelTestServer{call: server.CallTool}
	replies, err := dispatchBatch(context.Background(), srv, calls, Limits{ConcurrentToolCalls: 4, ToolResultBytes: 100}, 0)
	if err != nil || len(replies) != len(calls) {
		t.Fatalf("batch replies = %+v, err = %v", replies, err)
	}
	seen = server.seen()
	if len(seen) != len(calls) {
		t.Fatalf("called %+v", seen)
	}
	for i, call := range calls {
		if seen[i].Name != call.Name {
			t.Fatalf("called %+v, want %s at %d", seen, call.Name, i)
		}
	}
}
