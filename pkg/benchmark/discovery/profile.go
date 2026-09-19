package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ToolCall is one completed tool invocation, classified.
type ToolCall struct {
	Tool        string `json:"tool"`
	Class       Class  `json:"class"`
	Verb        string `json:"verb,omitempty"` // shell-shaped tools only
	InputBytes  int    `json:"input_bytes"`
	OutputBytes int    `json:"output_bytes"`
	DurationMs  int64  `json:"duration_ms"`
	Failed      bool   `json:"failed,omitempty"`
}

// NodeProfile is one node's tool activity plus whatever token spend the
// store can attribute to it.
//
// Tokens is the node's AUTHORITATIVE spend, summed from its turn
// checkpoints. TokensKnown is false when no checkpoint carried usage —
// in that case Tokens is zero and must not be read as "spent nothing".
type NodeProfile struct {
	NodeID string `json:"node_id"`
	Kind   string `json:"kind,omitempty"`
	Calls  int    `json:"calls"`
	// ByClass counts calls per class; CallsBeforeMutation counts the
	// calls that ran before this node's first mutating call.
	ByClass             map[Class]int `json:"by_class"`
	BeforeMutation      map[Class]int `json:"before_mutation"`
	CallsBeforeMutation int           `json:"calls_before_mutation"`
	Mutated             bool          `json:"mutated"`
	InputBytes          int           `json:"input_bytes"`
	OutputBytes         int           `json:"output_bytes"`
	DurationMs          int64         `json:"duration_ms"`
	Tokens              int           `json:"tokens"`
	TokensKnown         bool          `json:"tokens_known"`
	// Verbs counts the shell verbs this node ran; UnknownVerbs counts the
	// subset the table could not name. The second is the actionable one:
	// a verb high in that ranking is the table's next entry, and its size
	// is how much of the measurement is guesswork.
	Verbs        map[string]int `json:"verbs,omitempty"`
	UnknownVerbs map[string]int `json:"unknown_verbs,omitempty"`
}

// RunProfile is one run's nodes, with the run-level facts a report needs
// to say how representative the sample is.
type RunProfile struct {
	RunID        string        `json:"run_id"`
	WorkflowName string        `json:"workflow_name,omitempty"`
	Status       string        `json:"status,omitempty"`
	Nodes        []NodeProfile `json:"nodes"`
	// StartedNotFinished counts tool calls that opened and never
	// completed (a killed run, a cancelled node). They are excluded
	// from every other count and reported so the gap is visible.
	StartedNotFinished int `json:"started_not_finished"`
}

// ParseRun reads one run's event stream and turn checkpoints and returns
// its profile. Runs with no tool activity yield a profile with no nodes,
// not an error: "this run never used a tool" is a measurement.
func ParseRun(ctx context.Context, s store.RunStore, runID string) (*RunProfile, error) {
	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("discovery.ParseRun(%s): load run: %w", runID, err)
	}
	events, err := s.LoadEvents(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("discovery.ParseRun(%s): load events: %w", runID, err)
	}

	prof := &RunProfile{RunID: runID, WorkflowName: run.WorkflowName, Status: string(run.Status)}

	type pending struct {
		tool  string
		input []byte
	}
	var (
		open      = map[string]pending{} // tool_use_id → what tool_started saw
		perNode   = map[string]*NodeProfile{}
		iters     = map[string]map[int]bool{} // node → loop iterations observed
		nodeOrder []string
	)

	node := func(id string) *NodeProfile {
		n, ok := perNode[id]
		if !ok {
			n = &NodeProfile{
				NodeID:         id,
				ByClass:        map[Class]int{},
				BeforeMutation: map[Class]int{},
			}
			perNode[id] = n
			nodeOrder = append(nodeOrder, id)
		}
		return n
	}

	for _, evt := range events {
		switch evt.Type {
		case store.EventNodeStarted:
			n := node(evt.NodeID)
			if k, _ := evt.Data["kind"].(string); k != "" {
				n.Kind = k
			}
			if iters[evt.NodeID] == nil {
				iters[evt.NodeID] = map[int]bool{}
			}
			iters[evt.NodeID][intField(evt.Data, "iteration")] = true

		case store.EventToolStarted:
			id, _ := evt.Data["tool_use_id"].(string)
			if id == "" {
				continue // a tool node: its completion event carries everything
			}
			tool, _ := evt.Data["tool"].(string)
			open[id] = pending{tool: tool, input: rawInput(evt.Data)}

		case store.EventToolCalled, store.EventToolError:
			n := node(evt.NodeID)
			tool, _ := evt.Data["tool"].(string)
			input := rawInput(evt.Data)
			if id, _ := evt.Data["tool_use_id"].(string); id != "" {
				if p, ok := open[id]; ok {
					if tool == "" {
						tool = p.tool
					}
					if len(input) == 0 {
						input = p.input
					}
					delete(open, id)
				}
			}
			call := ToolCall{
				Tool:        tool,
				Class:       Classify(tool, input),
				Verb:        ShellVerb(input),
				InputBytes:  intField(evt.Data, "input_size"),
				OutputBytes: outputBytes(evt.Data),
				DurationMs:  int64(intField(evt.Data, "duration_ms")),
				Failed:      evt.Type == store.EventToolError,
			}
			n.record(call)
		}
	}
	prof.StartedNotFinished = len(open)

	turns := store.AsTurnStore(s)
	for _, id := range nodeOrder {
		n := perNode[id]
		if turns != nil {
			n.Tokens, n.TokensKnown = nodeTokens(ctx, turns, runID, id, iters[id])
		}
		prof.Nodes = append(prof.Nodes, *n)
	}
	return prof, nil
}

// record folds one call into the node's counters. The "before mutation"
// counters stop the moment the node's first mutating call lands — that
// boundary is the only phase split the event stream actually supports.
func (n *NodeProfile) record(c ToolCall) {
	n.Calls++
	n.ByClass[c.Class]++
	if c.Verb != "" {
		if n.Verbs == nil {
			n.Verbs = map[string]int{}
		}
		n.Verbs[c.Verb]++
		if c.Class == ClassUnknown {
			if n.UnknownVerbs == nil {
				n.UnknownVerbs = map[string]int{}
			}
			n.UnknownVerbs[c.Verb]++
		}
	}
	n.InputBytes += c.InputBytes
	n.OutputBytes += c.OutputBytes
	n.DurationMs += c.DurationMs
	if c.Class == ClassMutation {
		n.Mutated = true // the boundary itself is not "before" it
		return
	}
	if !n.Mutated {
		n.BeforeMutation[c.Class]++
		n.CallsBeforeMutation++
	}
}

// nodeTokens sums a node's turn-checkpoint usage across every loop
// iteration the event stream showed. Returns known=false when no
// checkpoint carried any usage at all, so the caller can tell "no spend
// recorded" from "spent nothing".
func nodeTokens(ctx context.Context, ts store.TurnStore, runID, nodeID string, iterations map[int]bool) (int, bool) {
	if len(iterations) == 0 {
		iterations = map[int]bool{0: true}
	}
	total, known := 0, false
	for iter := range iterations {
		list, err := ts.ListTurns(ctx, runID, nodeID, iter)
		if err != nil {
			continue // a node with no checkpoints is normal, not an error
		}
		for _, t := range list {
			if t == nil {
				continue
			}
			u := t.Usage
			sum := u.InputTokens + u.OutputTokens + u.AggregateTokens
			if sum > 0 {
				known = true
				total += sum
			}
		}
	}
	return total, known
}

// rawInput returns the tool input bytes an event carries, preferring the
// inline payload and falling back to the bounded preview a large input
// was reduced to. Returns nil when the event carries neither.
func rawInput(data map[string]any) []byte {
	if data == nil {
		return nil
	}
	for _, key := range []string{"input", "input_preview"} {
		switch v := data[key].(type) {
		case string:
			if v != "" {
				return []byte(v)
			}
		case map[string]any:
			return marshalMap(v)
		}
	}
	return nil
}

// marshalMap re-encodes a decoded JSON object so the classifier sees the
// same bytes whether the store inlined the input as a string or as a
// nested object. An object that cannot be re-encoded yields nil, which
// reads downstream as "input unreadable" rather than as a class.
func marshalMap(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func outputBytes(data map[string]any) int {
	if n := intField(data, "output_size"); n > 0 {
		return n
	}
	if s, ok := data["output"].(string); ok {
		return len(s)
	}
	return 0
}

func intField(data map[string]any, key string) int {
	if data == nil {
		return 0
	}
	switch v := data[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// SortNodes orders a profile's nodes by descending tool-call count, so a
// report's first rows are the ones that did the most work.
func SortNodes(nodes []NodeProfile) {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Calls > nodes[j].Calls })
}
