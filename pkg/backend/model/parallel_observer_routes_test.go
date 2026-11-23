package model

// Witnesses for #3521: call.parallel stayed serial on two production routes —
// the sandbox relay's event hooks and the store hooks' attached observers —
// because neither carried the concurrency attestation the dispatcher's gate
// demands. These tests pin the attestation AND the safety it claims:
//
//   - the relay route: the gate opens (TestSandboxRelayToolObserversAreAuditedForParallelDispatch),
//     whole event envelopes survive truly concurrent observers on the real
//     EnvelopeWriter (TestSandboxRelayToolEventsSurviveConcurrentDispatch), and a
//     real call.parallel batch overlaps through the exact relay chain the runner
//     installs (TestCodemodeParallelOverlapsThroughTheSandboxRelay);
//   - the store route: attached observers no longer close the gate
//     (TestStoreEventHooksStayParallelWithAttachedObservers) and are invoked
//     serially even when the hooks fire concurrently
//     (TestStoreHookObserversAreSerialisedUnderConcurrentFire).
//
// Mutation discipline: removing `toolObserversAudited: true` from
// SandboxRelayHooks reddens the first and third witnesses on their own
// assertions; reverting the store hooks' attestation reddens the fourth;
// unwrapping the observers' serialisation reddens the last under -race.
// The facts asserted travel with the results (tool_use_id), never with the
// arrival order, and every concurrency witness runs in both completion orders.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// relayedEventID reads (event type, tool_use_id) off one relayed envelope.
// The relay clamps and renames nothing here: tool_use_id crosses whole, and
// every integrity assertion below matches on it, never on arrival order.
func relayedEventID(env delegate.Envelope) (eventType, id string) {
	var data delegate.EventData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", ""
	}
	id, _ = data.Payload["tool_use_id"].(string)
	return data.Type, id
}

type relayRead struct {
	events []delegate.Envelope
	err    error
}

// drainRelayedEvents reads exactly want event envelopes off the pipe: the
// tool loop under test sends exactly that many, and a blocked write would
// hang the node. A malformed line fails the whole channel in production;
// here it surfaces as the read's error.
func drainRelayedEvents(r io.Reader, want int) relayRead {
	reader := delegate.NewEnvelopeReader(r)
	events := make([]delegate.Envelope, 0, want)
	for len(events) < want {
		env, err := reader.Read()
		if err != nil {
			return relayRead{events: events, err: fmt.Errorf("relay channel failed after %d/%d events: %w", len(events), want, err)}
		}
		if env.Type != delegate.EnvelopeEvent {
			return relayRead{events: events, err: fmt.Errorf("unexpected envelope type %q on the relay channel", env.Type)}
		}
		events = append(events, env)
	}
	return relayRead{events: events}
}

func TestSandboxRelayToolObserversAreAuditedForParallelDispatch(t *testing.T) {
	t.Parallel()
	hooks := SandboxRelayHooks(func(delegate.Envelope) error { return nil }, nil)
	opts := GenerationOptions{Hooks: NewDefaultLifecycleHooks(hooks)}
	applyHooks("node", 0, hooks, &opts)
	if !opts.toolObserversAudited {
		t.Fatal("the sandbox relay's tool observers are not attested concurrent-safe: " +
			"the dispatcher gate will keep every sandboxed call.parallel batch serial")
	}
	d, _ := newToolDispatcher(context.Background(), map[string]*GenerationTool{}, opts.OnToolStarted, opts.OnToolCall, opts.Hooks, nil, nil, opts.toolObserversAudited)
	if !d.parallel {
		t.Fatal("the dispatcher gate stayed closed on the attested relay route")
	}
}

func TestSandboxRelayToolEventsSurviveConcurrentDispatch(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"a-first", "b-first"} {
		t.Run(order, func(t *testing.T) {
			pr, pw := io.Pipe()
			writer := delegate.NewEnvelopeWriter(pw)
			const want = 4 // two nested calls, a started and a called each
			readDone := make(chan relayRead, 1)
			go func() { readDone <- drainRelayedEvents(pr, want) }()
			var reportErr atomic.Pointer[error]
			relay := SandboxRelayHooks(writer.Write, func(err error) {
				reportErr.CompareAndSwap(nil, &err)
			})

			// Two concurrent nested calls fire started+called each; the gate
			// channels force both completion orders, so a witness that only
			// survives one interleave cannot hide.
			gateA, gateB := make(chan struct{}), make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-gateA
				relay.OnToolStarted("node", LLMToolStartedInfo{ToolName: "glob", ToolUseID: "id-a"})
				relay.OnToolCall("node", LLMToolCallInfo{ToolName: "glob", ToolUseID: "id-a"})
			}()
			go func() {
				defer wg.Done()
				<-gateB
				relay.OnToolStarted("node", LLMToolStartedInfo{ToolName: "workspace_grep", ToolUseID: "id-b"})
				relay.OnToolCall("node", LLMToolCallInfo{ToolName: "workspace_grep", ToolUseID: "id-b"})
			}()
			if order == "a-first" {
				close(gateA)
				time.Sleep(time.Millisecond)
				close(gateB)
			} else {
				close(gateB)
				time.Sleep(time.Millisecond)
				close(gateA)
			}
			wg.Wait()
			if err := pw.Close(); err != nil {
				t.Fatalf("close relay writer: %v", err)
			}
			out := <-readDone
			if out.err != nil {
				t.Fatalf("relay reader failed: %v", out.err)
			}
			if reportErr.Load() != nil {
				t.Fatalf("the relay reported a lost observation: %v", *reportErr.Load())
			}
			started, completed := map[string]string{}, map[string]bool{}
			for _, env := range out.events {
				eventType, id := relayedEventID(env)
				if id == "" {
					t.Fatalf("relay event without its tool_use_id: %s", env.Data)
				}
				switch eventType {
				case string(store.EventToolStarted):
					if started[id] != "" {
						t.Fatalf("duplicate start for %s", id)
					}
					started[id] = "started"
				case string(store.EventToolCalled):
					if started[id] == "" || completed[id] {
						t.Fatalf("completion for %s does not follow its own start", id)
					}
					completed[id] = true
				default:
					t.Fatalf("unexpected relayed event type %q", eventType)
				}
			}
			if len(started) != 2 || len(completed) != 2 {
				t.Fatalf("observed starts=%v completions=%v, want both calls seen whole", started, completed)
			}
		})
	}
}

// parallelBatchFixture registers the two concurrent-safe builtins the overlap
// witnesses drive, exactly the way the auditor test does: the first read-scope
// call blocks until the second enters, so the witness only passes when the
// batch truly overlaps.
func parallelBatchFixture(t *testing.T) ([]delegate.ToolDef, *atomic.Bool) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "fixture.txt"), []byte("expected payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstTwo := make(chan struct{})
	var calls atomic.Int32
	var timedOut atomic.Bool
	scope := tool.NewReadScopeWithProvider(func() string {
		switch calls.Add(1) {
		case 1:
			select {
			case <-firstTwo:
			case <-time.After(3 * time.Second):
				timedOut.Store(true)
			}
		case 2:
			close(firstTwo)
		}
		return workspace
	})
	registry := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithScope(registry, scope, nil); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterClawWorkspaceDiagnosticsWithScope(registry, scope, nil); err != nil {
		t.Fatal(err)
	}
	var defs []delegate.ToolDef
	for _, name := range []string{"glob", "workspace_grep", "read_file", "grep", "bash"} {
		def, err := registry.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		defs = append(defs, def.ToDelegateDef())
	}
	return defs, &timedOut
}

// runParallelBatch drives one call.parallel batch through the door the way
// the generation loop does and returns the nested calls' results. It fails
// the test when the dispatcher gate refused to open, because the sync trick
// below would then wait out its whole window on a serial batch.
func runParallelBatch(t *testing.T, opts GenerationOptions, globFirst bool) {
	t.Helper()
	d, ctx := newToolDispatcher(context.Background(), buildToolMap(opts.Tools), opts.OnToolStarted, opts.OnToolCall, opts.Hooks, opts.MaterializeSecrets, nil, opts.toolObserversAudited)
	if !d.parallel {
		t.Fatal("the dispatcher gate stayed closed: the batch cannot overlap, so the " +
			"first builtin waits out its 3 s sync window on a serial batch")
	}
	const glob = `{name: "glob", input: {pattern: "*.txt"}}`
	const grep = `{name: "workspace_grep", input: {pattern: "expected payload"}}`
	first, second := glob, grep
	if !globFirst {
		first, second = second, first
	}
	input, err := json.Marshal(map[string]string{"source": "call.parallel([" + first + "," + second + "])"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := codemodeTool(childHost(), scripthost.Limits{Memory: 1 << 30, Wall: 10 * time.Second, ConcurrentToolCalls: 2}, codemodeDescription).Execute(ctx, input)
	if err != nil {
		t.Fatalf("codemode execute: %v", err)
	}
	var results []struct{ Status, Text string }
	if err := json.Unmarshal([]byte(out), &results); err != nil || len(results) != 2 {
		t.Fatalf("batch results = %s, error = %v", out, err)
	}
	want := []string{"fixture.txt", "expected payload"}
	if !globFirst {
		want[0], want[1] = want[1], want[0]
	}
	for i, text := range want {
		if results[i].Status != "ok" || !strings.Contains(results[i].Text, text) {
			t.Fatalf("result %d = %+v, want successful result containing %q", i, results[i], text)
		}
	}
}

func assertRelayedBatchWhole(t *testing.T, out relayRead) {
	t.Helper()
	if out.err != nil {
		t.Fatalf("relay reader failed: %v", out.err)
	}
	started, completed := map[string]string{}, map[string]bool{}
	for _, env := range out.events {
		eventType, id := relayedEventID(env)
		if id == "" {
			t.Fatalf("relay event without its tool_use_id: %s", env.Data)
		}
		switch eventType {
		case string(store.EventToolStarted):
			if started[id] != "" {
				t.Fatalf("duplicate start for %s", id)
			}
			started[id] = "started"
		case string(store.EventToolCalled):
			if started[id] == "" || completed[id] {
				t.Fatalf("completion for %s does not follow its own start", id)
			}
			completed[id] = true
		default:
			t.Fatalf("unexpected relayed event type %q", eventType)
		}
	}
	if len(started) != 2 || len(completed) != 2 {
		t.Fatalf("observed starts=%v completions=%v, want both nested calls relayed whole", started, completed)
	}
}

func TestCodemodeParallelOverlapsThroughTheSandboxRelay(t *testing.T) {
	t.Parallel()
	for _, globFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("glob-first=%t", globFirst), func(t *testing.T) {
			defs, timedOut := parallelBatchFixture(t)
			pr, pw := io.Pipe()
			writer := delegate.NewEnvelopeWriter(pw)
			// The exact chain a sandboxed runner installs: relay hooks whose
			// writes cross the runner's IPC envelope writer. The nested calls
			// relay four events (two started, two called); the reader drains
			// them while Execute runs, so a blocked write would hang the batch.
			readDone := make(chan relayRead, 1)
			go func() { readDone <- drainRelayedEvents(pr, 4) }()
			var reportErr atomic.Pointer[error]
			hooks := SandboxRelayHooks(writer.Write, func(err error) {
				reportErr.CompareAndSwap(nil, &err)
			})
			opts := GenerationOptions{Tools: toolDefsToGeneration(defs), Hooks: NewDefaultLifecycleHooks(hooks)}
			applyHooks("node", 0, hooks, &opts)
			runParallelBatch(t, opts, globFirst)
			if err := pw.Close(); err != nil {
				t.Fatalf("close relay writer: %v", err)
			}
			assertRelayedBatchWhole(t, <-readDone)
			if timedOut.Load() {
				t.Fatal("the first builtin waited out its sync window: the batch ran serial through the sandbox relay")
			}
			if reportErr.Load() != nil {
				t.Fatalf("the relay reported a lost observation: %v", *reportErr.Load())
			}
		})
	}
}

func TestStoreEventHooksStayParallelWithAttachedObservers(t *testing.T) {
	t.Parallel()
	for _, globFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("glob-first=%t", globFirst), func(t *testing.T) {
			defs, timedOut := parallelBatchFixture(t)
			var mu sync.Mutex
			var events []store.Event
			emitter := codemodeEventEmitter(func(event store.Event) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, event)
			})
			// The runtime-owned shapes an ADR-046 launch attaches: the
			// broker's non-reentrant publish, plus one observer advancing a
			// watermark (the dispatcher's stall heartbeat).
			var published, observed atomic.Int32
			publish := func(store.Event) { published.Add(1) }
			observer := func(store.Event) { observed.Add(1) }
			hooks := NewStoreEventHooksWithBroker(context.Background(), emitter, "run", nil, nil, nil, publish, observer)
			opts := GenerationOptions{Tools: toolDefsToGeneration(defs), Hooks: NewDefaultLifecycleHooks(hooks)}
			applyHooks("node", 0, hooks, &opts)
			if !opts.toolObserversAudited {
				t.Fatal("store hooks with attached observers are not attested: " +
					"every dispatched run would keep call.parallel serial")
			}
			runParallelBatch(t, opts, globFirst)
			if timedOut.Load() {
				t.Fatal("the first builtin waited out its sync window: the batch ran serial with observers attached")
			}
			mu.Lock()
			defer mu.Unlock()
			started, completed := 0, 0
			for _, event := range events {
				id, _ := event.Data["tool_use_id"].(string)
				if id == "" {
					t.Fatalf("store event without its tool_use_id: %+v", event)
				}
				switch event.Type {
				case store.EventToolStarted:
					started++
				case store.EventToolCalled:
					completed++
				}
			}
			if started != 2 || completed != 2 {
				t.Fatalf("store saw starts=%d completions=%d, want both nested calls persisted", started, completed)
			}
			if got := observed.Load(); got != int32(started+completed) {
				t.Fatalf("watermark observer saw %d events, the store persisted %d", got, started+completed)
			}
			if got := published.Load(); got != int32(started+completed) {
				t.Fatalf("broker saw %d events, the store persisted %d", got, started+completed)
			}
		})
	}
}

func TestStoreHookObserversAreSerialisedUnderConcurrentFire(t *testing.T) {
	t.Parallel()
	// Deliberately unguarded observers: the hooks layer must serialise the
	// invocations itself, because a future launch may attach any runtime-owned
	// callback and the attestation must hold by construction, not by each
	// observer's own discipline. Under -race an unwrapped fan-out reddens here.
	var observed []int
	obsA := func(store.Event) { observed = append(observed, 1) }
	obsB := func(store.Event) { observed = append(observed, 2) }
	hooks := NewStoreEventHooks(context.Background(), codemodeEventEmitter(func(store.Event) {}), "run", nil, nil, nil, obsA, obsB)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range 2 {
		go func() {
			defer wg.Done()
			hooks.OnToolStarted("node", LLMToolStartedInfo{ToolName: "glob", ToolUseID: fmt.Sprintf("id-%d", i)})
		}()
	}
	wg.Wait()
	if len(observed) != 4 {
		t.Fatalf("observers saw %d invocations, want 2 events x 2 observers", len(observed))
	}
}
