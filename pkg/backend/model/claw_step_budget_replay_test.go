package model

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// The charge read from the replay, where the replay is not the conversation
// it started from: compacted, healed, or answered from (#3283).

// sizedStoredRounds is a persisted session of n completed probe turns whose
// tool results are size random bytes in hex — incompressible, so a large
// size forces the threshold compaction to fold the oldest turns first.
func sizedStoredRounds(n, size int) []api.Message {
	r := rand.New(rand.NewSource(int64(n*7919 + size)))
	conversation := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Earlier node of the slot."}}}}
	for i := 1; i <= n; i++ {
		buf := make([]byte, size)
		r.Read(buf)
		id := fmt.Sprintf("tu_prior_%d", i)
		conversation = append(conversation,
			api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: id, Name: "probe", Input: map[string]any{}}}},
			api.Message{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: id, Content: hex.EncodeToString(buf)}.ToContentBlock()}},
		)
	}
	return conversation
}

// A persisted session of 6 turns, the node's own 3 turns, then a cut: the
// retry replays the dead attempt's 3 turns verbatim. With large earlier
// results the compaction folds the session's own turns first — the anchor
// among them — and the retry is still charged the 3 it sees.
//
// Mutant: charge by count (the completed turns beyond the starting
// conversation's count of 6): the compacted retry shows 3, is charged
// nothing, and runs the whole bound again — 8 for 5.
func TestClawStepBudget_ACompactionThatFoldsTheStartKeepsTheDeadTurnsCharged(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name      string
		size      int
		compacted bool
	}{{"no compaction", 10, false}, {"the start compacted away", 15000, true}} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			const bound = 5
			probe, ran := budgetProbe()
			client := budgetModel(map[int]bool{4: true}, nil)
			store := newNodeSessionStore()
			store.save("run-budget", "judge", sizedStoredRounds(6, arm.size))
			res, err := budgetBackend(client, 2).Execute(withRuntimeContext(context.Background(), "run-budget", store), budgetTask(probe, bound))
			requireVerdict(t, res, err)
			folded := len(client.calls[1].Messages) < len(client.calls[0].Messages)
			if folded != arm.compacted {
				t.Fatalf("compaction folded the start = %v, want %v (request 1: %d messages, request 2: %d)",
					folded, arm.compacted, len(client.calls[0].Messages), len(client.calls[1].Messages))
			}
			if got := ran.Load(); got != bound {
				t.Fatalf("%d tool turns ran for tool_max_steps %d (requests %s)", got, bound, requestKinds(client))
			}
		})
	}
}

// pausedConversation is a conversation paused on tu_ask, the call blocks
// put beside it in the order given.
func pausedConversation(blocks ...api.ContentBlock) json.RawMessage {
	prior, _ := json.Marshal([]api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Judge the diff."}}},
		{Role: "assistant", Content: blocks},
	})
	return prior
}

var (
	askBlock     = api.ContentBlock{Type: "tool_use", ID: "tu_ask", Name: "ask_user", Input: map[string]any{"question": "go?"}}
	siblingBlock = api.ContentBlock{Type: "tool_use", ID: "tu_sib", Name: "probe", Input: map[string]any{}}
)

// A resumed conversation is healed by the loop before its first request —
// a call left beside the paused one without a result is dropped, a
// command narrowing the conversation no longer states is restated — so its
// capture begins with the healed form. The retry must still recognise the
// capture as its own: it replays it, sees the 4 turns, and is charged them.
//
// Mutants: recognise a resumed capture by its prefix as carried, and every
// healed resume restarts from the pause point with the whole bound — 8 for
// 4; compare a continued conversation as carried only, and the narrowed one
// restarts the same way.
func TestClawStepBudget_AHealedResumeIsRetriedFromItsCapture(t *testing.T) {
	t.Parallel()
	continued, _ := json.Marshal([]api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Judge the diff."}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "verdict: maybe"}}},
	})
	for _, tc := range []struct {
		name      string
		prior     json.RawMessage
		continued bool
		narrowed  bool
	}{
		{name: "a sibling before the paused call", prior: pausedConversation(siblingBlock, askBlock)},
		{name: "a sibling after the paused call", prior: pausedConversation(askBlock, siblingBlock)},
		{name: "a narrowing the conversation no longer states", prior: pausedConversation(askBlock), narrowed: true},
		{name: "a continued conversation the loop restates a narrowing in", prior: continued, continued: true, narrowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe, ran := budgetProbe()
			client := budgetModel(map[int]bool{5: true}, nil)
			task := budgetTask(probe, 4)
			if tc.continued {
				task.ContinueConversation = tc.prior
			} else {
				task.ResumeConversation, task.ResumePendingToolUseID, task.ResumeAnswer = tc.prior, "tu_ask", "yes"
			}
			if tc.narrowed {
				task.ResumeCommandNarrowing = &delegate.CommandNarrowingState{Command: "review", Names: []string{"probe"}}
			}
			res, err := budgetBackend(client, 3).Execute(budgetCtx(), task)
			requireVerdict(t, res, err)
			if got := ran.Load(); got != 4 {
				t.Fatalf("%d tool turns ran for tool_max_steps 4 (requests %s)", got, requestKinds(client))
			}
			if got := requestKinds(client); got != "LLLLRR" {
				t.Errorf("requests = %s, want the 4 turns, the cut recovery pass and its retry", got)
			}
		})
	}
}

// A permission pause approved once, its gated call paused beside a sibling:
// the resumed attempt runs the approved deploy, probes, and its final
// answer is cut. The retry replays that attempt's capture — the deploy's
// result in view — so the approved action runs once.
//
// Mutant: the carried history compared as carried only — the retry restarts
// at the approval and runs the deploy again.
func TestClawStepBudget_AnApprovedActionRunsOnceAcrossARetry(t *testing.T) {
	t.Parallel()
	for _, sibling := range []bool{false, true} {
		t.Run(fmt.Sprint("sibling=", sibling), func(t *testing.T) {
			t.Parallel()
			probe, _ := budgetProbe()
			deploys := 0
			deploy := delegate.ToolDef{Name: "deploy", Description: "deploy to prod", InputSchema: json.RawMessage(`{"type":"object"}`),
				Execute: func(context.Context, json.RawMessage) (string, error) { deploys++; return "deployed", nil }}
			cut := false
			client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				if !offersTool(req, "probe") {
					return toolUseEvents("so_1", "structured_output", `{"verdict":"done"}`, 10, 5), nil
				}
				seen := func(id string) bool {
					return slices.ContainsFunc(req.Messages, func(m api.Message) bool {
						return slices.ContainsFunc(m.Content, func(b api.ContentBlock) bool { return b.Type == "tool_result" && b.ToolUseID == id })
					})
				}
				switch {
				case !seen("tu_redeploy"):
					return toolUseEvents("tu_redeploy", "deploy", `{}`, 10, 5), nil
				case !seen("tu_probe"):
					return toolUseEvents("tu_probe", "probe", `{}`, 10, 5), nil
				case !cut:
					cut = true
					return cutStream(), nil
				}
				return textEvents(`{"verdict":"done"}`, 10, 5), nil
			}}
			pol, err := permission.NewPolicy(permission.ModeAsk, []string{"probe"}, []string{"deploy"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			pol.AddGrantRule("deploy")
			blocks := []api.ContentBlock{{Type: "tool_use", ID: "tu_dep", Name: "deploy", Input: map[string]any{}}}
			if sibling {
				blocks = append([]api.ContentBlock{siblingBlock}, blocks...)
			}
			task := delegate.Task{NodeID: "judge", Model: "test/test-model", UserPrompt: "Ship it.",
				HasTools: true, ToolDefs: []delegate.ToolDef{probe, deploy}, ToolMaxSteps: 6,
				OutputSchema: json.RawMessage(budgetSchema), Permission: pol,
				ResumeConversation: pausedConversation(blocks...), ResumePendingToolUseID: "tu_dep", ResumeAnswer: "allow once"}
			res, err := budgetBackend(client, 3).Execute(budgetCtx(), task)
			requireVerdict(t, res, err)
			if deploys != 1 {
				t.Fatalf("the approved deploy ran %d times across the retry (requests %s), want once", deploys, requestKinds(client))
			}
		})
	}
}

// A retry whose replay already holds a completed tool turn of this node
// execution has its grounding in view: it is not forced to call a tool
// first. An always-thinking model that answers from what it sees on the
// retry answers (it is not refused for calling no tool on this
// invocation); a model under tool_choice gets no forced "any" on the
// retry's first request.
//
// Mutant: keep ForceInitialToolUse on a charged loop — the always-thinking
// retry fails with "returned before executing a required initial tool",
// and the other retry's first request carries tool_choice "any".
func TestClawStepBudget_AReplayedRetryIsNotForcedToCallAToolAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model   string
		resumed bool
	}{
		{"test/claude-opus-5-5", false},
		{"test/claude-opus-5-5", true},
		{"test/test-model", false},
	} {
		t.Run(fmt.Sprintf("%s resumed=%v", tc.model, tc.resumed), func(t *testing.T) {
			t.Parallel()
			probe, _ := budgetProbe()
			cutDone := false
			var retryFirst *api.CreateMessageRequest
			client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				if !offersTool(req, "probe") {
					return toolUseEvents("so_1", "structured_output", `{"verdict":"done"}`, 10, 5), nil
				}
				seen := 0
				for _, m := range req.Messages {
					for _, b := range m.Content {
						if b.Type == "tool_result" && strings.HasPrefix(b.ToolUseID, "tu_") && b.ToolUseID != "tu_ask" {
							seen++
						}
					}
				}
				if seen >= 2 {
					if !cutDone {
						cutDone = true
						return cutStream(), nil
					}
					if retryFirst == nil {
						r := req
						retryFirst = &r
					}
					return textEvents(`{"verdict":"done"}`, 10, 5), nil
				}
				return toolUseEvents(fmt.Sprintf("tu_%d", n), "probe", `{}`, 10, 5), nil
			}}
			task := budgetTask(probe, 6)
			task.Model = tc.model
			if tc.resumed {
				task.ResumeConversation, task.ResumePendingToolUseID, task.ResumeAnswer = pausedConversation(askBlock), "tu_ask", "yes"
			}
			res, err := budgetBackend(client, 3).Execute(budgetCtx(), task)
			requireVerdict(t, res, err)
			if retryFirst == nil {
				t.Fatalf("the retry made no request (requests %s)", requestKinds(client))
			}
			if choice := retryFirst.ToolChoice; choice != nil && choice.Type == "any" {
				t.Errorf("the retry's first request forces tool_choice %q although its conversation holds this execution's turns", choice.Type)
			}
		})
	}
}

// roundsSink records, for each conversation a runner-local store mirrors,
// the completed tool turns it holds.
type roundsSink struct {
	mu     sync.Mutex
	rounds []int
}

func (s *roundsSink) Capture(_, _ string, snapshot []byte) {
	var conversation []api.Message
	_ = json.Unmarshal(snapshot, &conversation)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rounds = append(s.rounds, completedToolRounds(conversation))
}

// A sandboxed runner can die between two requests of its loop, before the
// attempt's own capture: the turns it completed reach the host only if they
// are captured as they complete. Each completed tool round is captured —
// 1, 2, 3 turns — before the cut attempt's own capture.
//
// Mutant: capture only at the end of an attempt, and a death mid-loop
// leaves the host none of its turns: the retry runs them again.
func TestClawStepBudget_EveryCompletedToolRoundIsCaptured(t *testing.T) {
	t.Parallel()
	probe, _ := budgetProbe()
	client := budgetModel(map[int]bool{4: true}, nil)
	sink := &roundsSink{}
	ctx := WithSandboxRunnerSession(context.Background(), "runner", NewNodeSessionStore(), sink)
	if _, err := budgetBackend(client, 1).Execute(ctx, budgetTask(probe, 5)); err == nil {
		t.Fatal("the cut must fail the only attempt")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !slices.Equal(sink.rounds, []int{1, 2, 3, 3}) {
		t.Fatalf("captures held %v completed tool turns, want each round as it completed — [1 2 3] — then the attempt's own [3]", sink.rounds)
	}
}

// bigProbe is budgetProbe answering results large enough that a few of them
// force the threshold compaction.
func bigProbe() (delegate.ToolDef, *atomic.Int64) {
	probe, ran := budgetProbe()
	inner := probe.Execute
	probe.Execute = func(ctx context.Context, input json.RawMessage) (string, error) {
		if _, err := inner(ctx, input); err != nil {
			return "", err
		}
		buf := make([]byte, 24000)
		rand.New(rand.NewSource(ran.Load())).Read(buf)
		return hex.EncodeToString(buf), nil
	}
	return probe, ran
}

// The node's own turns are large: the compaction folds the earliest of them
// before the stream is cut. The summary names what it folded, so the retry
// is charged all 4 turns — those it sees and those folded — and is left
// the 2 the bound allows.
//
// Mutant: drop the summary's record of the folded turns, and the retry is
// charged only the turns still in view: it runs past the bound.
func TestClawStepBudget_TurnsACompactionFoldsStayCharged(t *testing.T) {
	t.Parallel()
	const bound = 6
	probe, ran := bigProbe()
	client := budgetModel(map[int]bool{5: true}, nil)
	res, err := budgetBackend(client, 2).Execute(budgetCtx(), budgetTask(probe, bound))
	requireVerdict(t, res, err)
	if visible := toolResultsIn(client.calls[4]); visible >= 4 {
		t.Fatalf("scenario broken: the cut request still carries %d of the 4 tool results — nothing was folded", visible)
	}
	if got := ran.Load(); got != bound {
		t.Fatalf("%d tool turns ran for tool_max_steps %d (requests %s)", got, bound, requestKinds(client))
	}
}

// A resumed attempt whose large turns the compaction folded leaves a
// capture that no longer begins with the carried history; the paused
// call's answer, named in it, still marks it as this execution's. The
// retry replays it and is charged the 4 turns it completed.
//
// Mutant: recognise a resumed capture by its prefix alone, and the
// compacted capture is not recognised: the retry restarts from the pause
// with the whole bound.
func TestClawStepBudget_ACompactedResumeIsRetriedFromItsCapture(t *testing.T) {
	t.Parallel()
	const bound = 6
	probe, ran := bigProbe()
	client := budgetModel(map[int]bool{5: true}, nil)
	task := budgetTask(probe, bound)
	task.ResumeConversation, task.ResumePendingToolUseID, task.ResumeAnswer = pausedConversation(askBlock), "tu_ask", "yes"
	res, err := budgetBackend(client, 2).Execute(budgetCtx(), task)
	requireVerdict(t, res, err)
	if visible := toolResultsIn(client.calls[4]); visible >= 5 {
		t.Fatalf("scenario broken: the cut request still carries %d tool results — nothing was folded", visible)
	}
	if got := ran.Load(); got != bound {
		t.Fatalf("%d tool turns ran for tool_max_steps %d (requests %s)", got, bound, requestKinds(client))
	}
}
