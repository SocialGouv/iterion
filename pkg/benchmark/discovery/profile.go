package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ToolCall is one completed tool invocation, classified.
type ToolCall struct {
	Tool  string `json:"tool"`
	Class Class  `json:"class"`
	Verb  string `json:"verb,omitempty"` // shell-shaped tools only
	// UnnamedVerb is the verb of the first segment the table could not
	// name. It differs from Verb whenever a chain's head IS named and a
	// later segment is not — `grep -q x && <unnamed>` — and it is the
	// one a to-do list has to carry.
	UnnamedVerb string `json:"unnamed_verb,omitempty"`
	InputBytes  int    `json:"input_bytes"`
	OutputBytes int    `json:"output_bytes"`
	DurationMs  int64  `json:"duration_ms"`
	// ElapsedMs is the call's real elapsed time, taken from the gap
	// between its own two events. DurationMs is what the BACKEND
	// measured, and most backends measure nothing — the two are
	// different instruments and are never summed together.
	ElapsedMs int64 `json:"elapsed_ms"`
	// Each Known flag is set by the reader that saw the field, never
	// derived from the value: a no-argument tool records a real zero
	// input size, and a zero that means "unmeasured" summed next to it
	// is how a total becomes a fiction.
	InputKnown    bool `json:"input_known"`
	OutputKnown   bool `json:"output_known"`
	DurationKnown bool `json:"duration_known"`
	ElapsedKnown  bool `json:"elapsed_known"`
	Failed        bool `json:"failed,omitempty"`
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
	ElapsedMs           int64         `json:"elapsed_ms"`
	// Every summed numeric carries the count of calls that actually
	// contributed one. A sum without its denominator reads as a
	// measurement of every call, and on duration that was wrong by 6.7×.
	CallsWithInput    int  `json:"calls_with_input"`
	CallsWithOutput   int  `json:"calls_with_output"`
	CallsWithDuration int  `json:"calls_with_duration"`
	CallsWithElapsed  int  `json:"calls_with_elapsed"`
	Tokens            int  `json:"tokens"`
	TokensKnown       bool `json:"tokens_known"`
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
	// CompletionsWithoutStart is the mirror case, and it is the one the
	// dedup rides on: a completion with no open start is dropped as the
	// engine's duplicate. That reading is only true while every start is
	// present. If a truncated stream or a resume ever loses starts, real
	// calls are discarded here and the corpus totals slide DOWNWARD
	// while still looking like a measurement — so the count is published
	// rather than left to be inferred from a total that shrank.
	CompletionsWithoutStart int `json:"completions_without_start"`
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
		tool       string
		input      []byte
		inputBytes int
		inputKnown bool
		started    time.Time
	}
	var (
		open = map[string]pending{} // tool_use_id → what tool_started saw
		// Tool NODES (`shell:<id>`, `script:<lang>:<id>`) carry no
		// tool_use_id, and the engine emits TWO completions for each of
		// them — same duration, one with input_size 0 and one with the
		// real size. Measured on the operator's store: 542 starts against
		// 1 068 completions, a ratio of 1.97, while every other tool sits
		// at 0.98. Without a budget per (node, tool) each of those calls
		// was counted twice, inflating the call count, the unknown share
		// and 39 minutes of the published tool wall time.
		nodeOpen  = map[string][]pending{}
		perNode   = map[string]*NodeProfile{}
		iters     = map[string]map[int]bool{} // node → loop iterations observed
		nodeOrder []string
	)
	nodeKey := func(nodeID, tool string) string { return nodeID + "\x00" + tool }

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
			tool, _ := evt.Data["tool"].(string)
			startBytes, startKnown := intFieldPresent(evt.Data, "input_size")
			p := pending{
				tool: tool, input: rawInput(evt.Data),
				inputBytes: startBytes, inputKnown: startKnown,
				started: evt.Timestamp,
			}
			if id, _ := evt.Data["tool_use_id"].(string); id != "" {
				open[id] = p
				continue
			}
			key := nodeKey(evt.NodeID, tool)
			nodeOpen[key] = append(nodeOpen[key], p)

		case store.EventToolCalled, store.EventToolError:
			n := node(evt.NodeID)
			tool, _ := evt.Data["tool"].(string)
			input := rawInput(evt.Data)
			inputBytes, inputKnown := intFieldPresent(evt.Data, "input_size")
			var started time.Time

			if id, _ := evt.Data["tool_use_id"].(string); id != "" {
				if p, ok := open[id]; ok {
					if tool == "" {
						tool = p.tool
					}
					if len(input) == 0 {
						input = p.input
					}
					if p.inputBytes > inputBytes {
						inputBytes, inputKnown = p.inputBytes, p.inputKnown
					} else if !inputKnown {
						inputKnown = p.inputKnown
					}
					started = p.started
					delete(open, id)
				} else {
					prof.CompletionsWithoutStart++
				}
			} else {
				// One completion per start. A second completion for the
				// same (node, tool) has no start left to consume and is
				// the engine's duplicate, not a second call.
				key := nodeKey(evt.NodeID, tool)
				queue := nodeOpen[key]
				if len(queue) == 0 {
					prof.CompletionsWithoutStart++
					continue
				}
				p := queue[0]
				nodeOpen[key] = queue[1:]
				if len(input) == 0 {
					input = p.input
				}
				// The start carries the real input size; one of the two
				// completions reports 0, and taking the larger keeps the
				// bytes-pulled-in line true whichever arrives first.
				if p.inputBytes > inputBytes {
					inputBytes, inputKnown = p.inputBytes, p.inputKnown
				} else if !inputKnown {
					inputKnown = p.inputKnown
				}
				started = p.started
			}

			outBytes, outKnown := outputBytesPresent(evt.Data)
			durationMs, durationPresent := intFieldPresent(evt.Data, "duration_ms")
			// Presence cannot separate "fast" from "unmeasured" here: the
			// streaming path writes the key with a zero rather than
			// omitting it (`pkg/backend/model/executor.go` builds its
			// LLMToolCallInfo without a Duration), so 94 % of calls carry
			// a zero nobody measured. A real tool call cannot take 0 ms,
			// which makes `> 0` the honest proxy on THIS field — and only
			// on this one, which is why it is not the shared rule.
			durationKnown := durationPresent && durationMs > 0

			var elapsedMs int64
			elapsedKnown := !started.IsZero() && !evt.Timestamp.IsZero()
			if elapsedKnown {
				if d := evt.Timestamp.Sub(started); d >= 0 {
					elapsedMs = d.Milliseconds()
				} else {
					elapsedKnown = false
				}
			}

			cls := Classify(tool, input)
			call := ToolCall{
				Tool:          tool,
				Class:         cls,
				Verb:          ShellVerb(input),
				InputBytes:    inputBytes,
				OutputBytes:   outBytes,
				DurationMs:    int64(durationMs),
				ElapsedMs:     elapsedMs,
				InputKnown:    inputKnown,
				OutputKnown:   outKnown,
				DurationKnown: durationKnown,
				ElapsedKnown:  elapsedKnown,
				Failed:        evt.Type == store.EventToolError,
			}
			if cls == ClassUnknown {
				call.UnnamedVerb = UnnamedVerb(tool, input)
			}
			n.record(call)
		}
	}
	prof.StartedNotFinished = len(open)
	for _, queue := range nodeOpen {
		prof.StartedNotFinished += len(queue)
	}

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
	}
	// The unnamed list carries the verb of the segment that could NOT be
	// named, not the chain's head. Keyed by the head, half that list was
	// verbs the table already names, which made a to-do out of work
	// already done.
	if c.Class == ClassUnknown && c.UnnamedVerb != "" {
		if n.UnknownVerbs == nil {
			n.UnknownVerbs = map[string]int{}
		}
		n.UnknownVerbs[c.UnnamedVerb]++
	}
	n.InputBytes += c.InputBytes
	n.OutputBytes += c.OutputBytes
	n.DurationMs += c.DurationMs
	n.ElapsedMs += c.ElapsedMs
	if c.InputKnown {
		n.CallsWithInput++
	}
	if c.OutputKnown {
		n.CallsWithOutput++
	}
	if c.DurationKnown {
		n.CallsWithDuration++
	}
	if c.ElapsedKnown {
		n.CallsWithElapsed++
	}
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

// outputBytesPresent reads a call's output size and reports whether any
// producer recorded one. iterion's own `tool` nodes record neither
// `output_size` nor `output` outside trace logging, so every one of them
// answers (0, false) — which is why the corpus total must publish its
// denominator instead of reading as "these nodes returned nothing".
func outputBytesPresent(data map[string]any) (int, bool) {
	if n, ok := intFieldPresent(data, "output_size"); ok && n > 0 {
		return n, true
	}
	if s, ok := data["output"].(string); ok {
		return len(s), true
	}
	return 0, false
}

func intField(data map[string]any, key string) int {
	v, _ := intFieldPresent(data, key)
	return v
}

// intFieldPresent reads a numeric field and reports whether the producer
// wrote it at all.
//
// The distinction is load-bearing and it is the reader's to make, never
// the caller's: a tool that takes no arguments records a REAL zero input
// size, while a backend that never measured its call records nothing.
// Deriving "unknown" from `value == 0` downstream would collapse those
// two — which is exactly the conflation this package already refuses for
// tokens (`TokensKnown`) and committed on every other numeric.
func intFieldPresent(data map[string]any, key string) (int, bool) {
	if data == nil {
		return 0, false
	}
	switch v := data[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// SortNodes orders a profile's nodes by descending tool-call count, so a
// report's first rows are the ones that did the most work.
func SortNodes(nodes []NodeProfile) {
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Calls > nodes[j].Calls })
}
