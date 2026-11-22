package scripthost

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type memoryStateServer struct {
	recordingServer
	mu     sync.Mutex
	values map[string]json.RawMessage
}

func (s *memoryStateServer) StoreState(_ context.Context, key string, value json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = bytes.Clone(value)
	return nil
}

func (s *memoryStateServer) LoadState(_ context.Context, key string) (json.RawMessage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, found := s.values[key]
	return bytes.Clone(value), found, nil
}

func TestStateRoundTripsAcrossRealChildren(t *testing.T) {
	t.Parallel()
	srv := &memoryStateServer{values: map[string]json.RawMessage{}}
	host := toolHost(t, srv)
	first, err := host.Run(t.Context(), `[call.store("empty",null),call.store("object",{n:42}),call.store("large", "<".repeat(16382))]`, smallLimits())
	if err != nil || first.Value != `[true,true,true]` {
		t.Fatalf("store response: %+v, %v", first, err)
	}
	second, err := host.Run(t.Context(), `[call.load("empty")===null,call.load("missing")===undefined,call.load("object").n,call.load("large")==="<".repeat(16382)]`, smallLimits())
	if err != nil || second.Value != `[true,true,42,true]` {
		t.Fatalf("load response: %+v, %v", second, err)
	}
}

func TestStateAndToolCallsShareOneParentBudget(t *testing.T) {
	t.Parallel()
	srv := &memoryStateServer{values: map[string]json.RawMessage{}}
	input := bytes.NewBufferString("{\"type\":\"state_store\",\"id\":1,\"name\":\"first\",\"input\":null}\n{\"type\":\"call\",\"id\":2,\"name\":\"ordinary\"}\n{\"type\":\"state_store\",\"id\":3,\"name\":\"unused\",\"input\":null}\n")
	var out bytes.Buffer
	limits := DefaultLimits()
	limits.ToolCalls = 2
	served, err := serveToolCalls(t.Context(), input, &out, srv, limits)
	if err != nil || served != 2 {
		t.Fatalf("served %d, err %v", served, err)
	}
	dec := json.NewDecoder(&out)
	for i, typ := range []string{frameStateResult, FrameResult, FrameBudget} {
		var f Frame
		if err := dec.Decode(&f); err != nil || f.Type != typ || f.ID != int64(i+1) {
			t.Fatalf("reply %d: %+v, %v", i, f, err)
		}
	}
	if _, found, _ := srv.LoadState(t.Context(), "unused"); found {
		t.Fatal("over-budget state write reached the server")
	}
}

func TestStateRejectsKeysThatCannotRoundTripAsUTF8(t *testing.T) {
	t.Parallel()
	srv := &memoryStateServer{values: map[string]json.RawMessage{}}
	host := toolHost(t, srv)
	got, err := host.Run(t.Context(), `
		call.store("�", 1);
		let rejected = 0;
		try { call.store("😀".slice(0, 1), 2) } catch (e) { if (e instanceof TypeError) rejected++ }
		try { call.load("😀".slice(1)) } catch (e) { if (e instanceof TypeError) rejected++ }
		call.store("😀", 3);
		[rejected, call.load("�"), call.load("😀")];`, smallLimits())
	if err != nil || got.Value != `[2,1,3]` {
		t.Fatalf("UTF-8 key result: %+v, %v", got, err)
	}
}

func TestStateRejectsValuesThatCannotRoundTrip(t *testing.T) {
	t.Parallel()
	srv := &memoryStateServer{values: map[string]json.RawMessage{}}
	host := toolHost(t, srv)
	got, err := host.Run(t.Context(), `
		const half = "😀".slice(0, 1);
		const literal = "\\ud83d";
		let rejected = 0;
		for (const value of [{s: half}, {[half]: 1}, () => 1, Symbol()]) {
			try { call.store("bad", value) } catch (e) { if (e instanceof TypeError) rejected++ }
		}
		call.store("good", {s: "😀", literal});
		[rejected, call.load("bad") === undefined, call.load("good").s === "😀", call.load("good").literal === literal];`, smallLimits())
	if err != nil || got.Value != `[4,true,true,true]` {
		t.Fatalf("JSON value result: %+v, %v", got, err)
	}
}

func TestStateUsesStandardJSONProjection(t *testing.T) {
	t.Parallel()
	host := toolHost(t, &memoryStateServer{values: map[string]json.RawMessage{}})
	got, err := host.Run(t.Context(), `
		call.store("object", {missing: undefined, callback: () => 1, number: NaN});
		call.store("array", [undefined, Infinity]);
		[call.load("object"), call.load("array")];`, smallLimits())
	if err != nil || got.Value != `[{"number":null},[null,null]]` {
		t.Fatalf("JSON projection: %+v, %v", got, err)
	}
}

func TestStateUnavailableIsCatchable(t *testing.T) {
	t.Parallel()
	host := toolHost(t, &recordingServer{})
	got, err := host.Run(t.Context(), `try {call.load("key")} catch(e) {e.code}`, smallLimits())
	if err != nil || got.Value != `"state_unavailable"` {
		t.Fatalf("response: %+v, %v", got, err)
	}
}
