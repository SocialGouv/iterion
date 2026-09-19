package discovery

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

var runCounter int64

func tmpStore(t *testing.T) store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

func newRun(t *testing.T, s store.RunStore) string {
	t.Helper()
	id := fmt.Sprintf("test-discovery-%d", atomic.AddInt64(&runCounter, 1))
	run, err := s.CreateRun(context.Background(), id, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run.ID
}

func appendEvent(t *testing.T, s store.RunStore, runID string, e store.Event) {
	t.Helper()
	e.RunID = runID
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if _, err := s.AppendEvent(context.Background(), runID, e); err != nil {
		t.Fatalf("AppendEvent(%s): %v", e.Type, err)
	}
}

// toolCall seeds the pair the engine really writes: tool_started carries
// the input, tool_called carries the outcome.
func toolCall(t *testing.T, s store.RunStore, runID, nodeID, useID, tool, input string) {
	t.Helper()
	appendEvent(t, s, runID, store.Event{
		Type: store.EventToolStarted, NodeID: nodeID,
		Data: map[string]any{"tool": tool, "tool_use_id": useID, "input": input, "input_size": len(input)},
	})
	appendEvent(t, s, runID, store.Event{
		Type: store.EventToolCalled, NodeID: nodeID,
		Data: map[string]any{"tool": tool, "tool_use_id": useID, "duration_ms": 10, "output": "ok"},
	})
}

// The phase boundary is the node's FIRST mutating call, and the write
// itself is not counted as "before" it. Moving the boundary by one
// reddens this test.
func TestPhaseBoundaryIsTheFirstWrite(t *testing.T) {
	s := tmpStore(t)
	runID := newRun(t, s)
	appendEvent(t, s, runID, store.Event{
		Type: store.EventNodeStarted, NodeID: "dev",
		Data: map[string]any{"kind": "agent", "iteration": 0},
	})
	toolCall(t, s, runID, "dev", "t1", "Read", `{"file_path":"a.go"}`)
	toolCall(t, s, runID, "dev", "t2", "Grep", `{"pattern":"x"}`)
	toolCall(t, s, runID, "dev", "t3", "Edit", `{"file_path":"a.go"}`)
	toolCall(t, s, runID, "dev", "t4", "Read", `{"file_path":"b.go"}`)

	prof, err := ParseRun(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	if len(prof.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(prof.Nodes))
	}
	n := prof.Nodes[0]
	if n.Calls != 4 {
		t.Fatalf("calls = %d, want 4", n.Calls)
	}
	if n.ByClass[ClassDiscovery] != 3 {
		t.Fatalf("discovery calls = %d, want 3", n.ByClass[ClassDiscovery])
	}
	if !n.Mutated {
		t.Fatal("node wrote and is not marked as mutating")
	}
	if n.CallsBeforeMutation != 2 {
		t.Fatalf("calls before the first write = %d, want 2 (the write is the boundary, not part of it)",
			n.CallsBeforeMutation)
	}
	if n.Kind != "agent" {
		t.Fatalf("kind = %q, want %q", n.Kind, "agent")
	}
}

// The class of a call comes from the input the STARTED event carried:
// the completion event alone has no command to read. Dropping the merge
// turns every shell call into "unknown".
func TestClassComesFromTheStartedInput(t *testing.T) {
	s := tmpStore(t)
	runID := newRun(t, s)
	appendEvent(t, s, runID, store.Event{
		Type: store.EventToolStarted, NodeID: "n",
		Data: map[string]any{"tool": "Bash", "tool_use_id": "u1", "input": `{"command":"git commit -m x"}`},
	})
	appendEvent(t, s, runID, store.Event{
		Type: store.EventToolCalled, NodeID: "n",
		Data: map[string]any{"tool": "Bash", "tool_use_id": "u1", "duration_ms": 5},
	})

	prof, err := ParseRun(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	n := prof.Nodes[0]
	if n.ByClass[ClassMutation] != 1 {
		t.Fatalf("mutation calls = %d, want 1 — the started event's command was lost", n.ByClass[ClassMutation])
	}
	if n.Verbs["git commit"] != 1 {
		t.Fatalf("verbs = %v, want git commit counted once", n.Verbs)
	}
}

// A call that opened and never completed is counted apart, not folded
// into the totals: a killed run must not inflate the activity it did.
func TestUnfinishedCallsAreCountedApart(t *testing.T) {
	s := tmpStore(t)
	runID := newRun(t, s)
	appendEvent(t, s, runID, store.Event{
		Type: store.EventToolStarted, NodeID: "n",
		Data: map[string]any{"tool": "Read", "tool_use_id": "u1", "input": `{"file_path":"a"}`},
	})

	prof, err := ParseRun(context.Background(), s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	if prof.StartedNotFinished != 1 {
		t.Fatalf("started-not-finished = %d, want 1", prof.StartedNotFinished)
	}
	for _, n := range prof.Nodes {
		if n.Calls != 0 {
			t.Fatalf("node %s counted %d calls for a call that never completed", n.NodeID, n.Calls)
		}
	}
}

// A node with no recorded usage must read as "unknown", never as zero:
// an unattributed spend folded into a ratio is a fabricated number.
//
// Two ways to have no usage, and they fail differently: no checkpoint at
// all, and a checkpoint whose Usage block is empty. Only the second
// catches a reader that marks the spend known before looking at it —
// 7 of the 500 checkpoints in the operator's own store are of that shape.
func TestMissingUsageIsUnknownNotZero(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		writeTurn bool
	}{
		{"no checkpoint at all", false},
		{"a checkpoint whose usage block is empty", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			runID := newRun(t, s)
			appendEvent(t, s, runID, store.Event{
				Type: store.EventNodeStarted, NodeID: "looker",
				Data: map[string]any{"kind": "agent", "iteration": 0},
			})
			toolCall(t, s, runID, "looker", "t1", "Read", `{"file_path":"a.go"}`)
			if tc.writeTurn {
				ts := store.AsTurnStore(s)
				if err := ts.WriteTurn(ctx, &store.TurnCheckpoint{
					RunID: runID, NodeID: "looker", LoopIter: 0, TurnIndex: 0,
					Backend: "claude_code", // no Usage: the backend reported none
				}); err != nil {
					t.Fatalf("WriteTurn: %v", err)
				}
			}

			prof, err := ParseRun(ctx, s, runID)
			if err != nil {
				t.Fatalf("ParseRun: %v", err)
			}
			if prof.Nodes[0].TokensKnown {
				t.Fatal("a node with no recorded usage reported a known token spend")
			}
			c := Aggregate([]*RunProfile{prof})
			if c.NodesWithoutTokens != 1 {
				t.Fatalf("nodes without tokens = %d, want 1", c.NodesWithoutTokens)
			}
			if c.AttributableTokens() != 0 {
				t.Fatalf("attributable tokens = %d, want 0", c.AttributableTokens())
			}
		})
	}
}

// A node that never mutated carries its WHOLE spend on the orientation
// side; a node that wrote carries its spend on the other, unsplit.
func TestPureDiscoveryNodeCarriesItsWholeSpend(t *testing.T) {
	s := tmpStore(t)
	ts := store.AsTurnStore(s)
	if ts == nil {
		t.Fatal("the filesystem store no longer satisfies TurnStore")
	}
	runID := newRun(t, s)
	ctx := context.Background()

	for _, n := range []struct {
		id     string
		tool   string
		input  string
		tokens int
	}{
		{"surveyor", "Read", `{"file_path":"a.go"}`, 1200},
		{"writer", "Edit", `{"file_path":"a.go"}`, 800},
	} {
		appendEvent(t, s, runID, store.Event{
			Type: store.EventNodeStarted, NodeID: n.id,
			Data: map[string]any{"kind": "agent", "iteration": 0},
		})
		toolCall(t, s, runID, n.id, "u-"+n.id, n.tool, n.input)
		if err := ts.WriteTurn(ctx, &store.TurnCheckpoint{
			RunID: runID, NodeID: n.id, LoopIter: 0, TurnIndex: 0,
			Backend: "claude_code",
			Usage:   store.TurnUsage{AggregateTokens: n.tokens},
		}); err != nil {
			t.Fatalf("WriteTurn(%s): %v", n.id, err)
		}
	}

	prof, err := ParseRun(ctx, s, runID)
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	c := Aggregate([]*RunProfile{prof})
	if c.PureDiscoveryNodes != 1 || c.PureDiscoveryTokens != 1200 {
		t.Fatalf("pure-discovery = %d nodes / %d tokens, want 1 / 1200",
			c.PureDiscoveryNodes, c.PureDiscoveryTokens)
	}
	if c.MutatingNodes != 1 || c.MutatingTokens != 800 {
		t.Fatalf("mutating = %d nodes / %d tokens, want 1 / 800",
			c.MutatingNodes, c.MutatingTokens)
	}
	if got := c.AttributableTokens(); got != 2000 {
		t.Fatalf("attributable = %d, want 2000", got)
	}
}

// The report states coverage before it states findings, and an empty
// corpus must say the share is undefined rather than print 0%.
func TestReportSaysUndefinedRatherThanZero(t *testing.T) {
	md := RenderMarkdown(Aggregate(nil), nil, RenderOptions{GeneratedAt: time.Now()})
	if !contains(md, "undefined, not zero") {
		t.Fatalf("an empty corpus rendered a ratio instead of declaring it undefined:\n%s", md)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
