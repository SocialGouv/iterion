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
	clawrt "github.com/SocialGouv/iterion/pkg/claw/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The charge read from the replay, where the replay is not the conversation
// it started from — compacted, captured across a runner, its call ids reused
// — and the fold record that keeps it countable (#3283).

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

var askBlock = api.ContentBlock{Type: "tool_use", ID: "tu_ask", Name: "ask_user", Input: map[string]any{"question": "go?"}}

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

// storedRoundsWithIDs is a stored conversation whose completed probe turns
// use the given tool call ids, in order, each turn narrated.
func storedRoundsWithIDs(ids ...string) []api.Message {
	conversation := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Earlier node of the slot."}}}}
	for i, id := range ids {
		conversation = append(conversation,
			api.Message{Role: "assistant", Content: []api.ContentBlock{
				{Type: "text", Text: fmt.Sprintf("previous execution narration %d", i+1)},
				{Type: "tool_use", ID: id, Name: "probe", Input: map[string]any{}}}},
			api.Message{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: id, Content: "observed"}.ToContentBlock()}},
		)
	}
	return conversation
}

// A provider may reuse a call id from one response to the next ("call_0" on
// every turn): the anchor names the LAST occurrence of its key, so a fresh
// execution on a conversation that holds earlier turns under the reused id
// is charged none of them — it runs the whole bound, forced to ground its
// first turn, whether the node answers through the recovery pass or ends
// at the bound. A resume after a human pause on such a route gets the
// whole bound as well.
//
// Mutant: resolve the anchor at the FIRST occurrence of its key, and every
// reused-id case is charged the earlier turns: starved, the schemaless one
// answering with the previous execution's text without a request.
func TestClawStepBudget_AReusedCallIDStarvesNoExecution(t *testing.T) {
	t.Parallel()
	reused := func(n int) []string { return slices.Repeat([]string{"call_0"}, n) }
	for _, tc := range []struct {
		name   string
		stored []string
		reuse  bool
		schema bool
	}{
		{name: "unique ids", stored: []string{"a1", "a2", "a3"}, schema: true},
		{name: "a reused id, 3 earlier turns", stored: reused(3), reuse: true, schema: true},
		{name: "a reused id, 7 earlier turns", stored: reused(7), reuse: true, schema: true},
		{name: "a reused id, 7 earlier turns, no output schema", stored: reused(7), reuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe, ran := budgetProbe()
			client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				if offersTool(req, "probe") {
					id := fmt.Sprintf("tu_new_%d", n)
					if tc.reuse {
						id = "call_0"
					}
					return toolUseEvents(id, "probe", `{}`, 10, 5), nil
				}
				return toolUseEvents("so_1", "structured_output", `{"verdict":"done"}`, 10, 5), nil
			}}
			task := budgetTask(probe, 5)
			if !tc.schema {
				task.OutputSchema = nil
			}
			store := newNodeSessionStore()
			store.save("run-budget", "judge", storedRoundsWithIDs(tc.stored...))
			if _, err := budgetBackend(client, 1).Execute(withRuntimeContext(context.Background(), "run-budget", store), task); err != nil {
				t.Fatalf("the node failed: %v", err)
			}
			if got := ran.Load(); got != 5 {
				t.Fatalf("a fresh execution with tool_max_steps 5 ran %d tool turns (requests %s)", got, requestKinds(client))
			}
			if choice := client.calls[0].ToolChoice; choice == nil || choice.Type != "any" {
				t.Errorf("the first request is not forced to call a tool (tool_choice %v): nothing of this execution grounds it yet", choice)
			}
		})
	}
	t.Run("a resume after a human pause, ids reused", func(t *testing.T) {
		t.Parallel()
		probe, ran := budgetProbe()
		conversation := storedRoundsWithIDs(reused(4)...)
		conversation = append(conversation, api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "call_0", Name: "ask_user", Input: map[string]any{"question": "go?"}}}})
		prior, _ := json.Marshal(conversation)
		client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
			if offersTool(req, "probe") {
				return toolUseEvents("call_0", "probe", `{}`, 10, 5), nil
			}
			return toolUseEvents("so_1", "structured_output", `{"verdict":"done"}`, 10, 5), nil
		}}
		task := budgetTask(probe, 5)
		task.ResumeConversation, task.ResumePendingToolUseID, task.ResumeAnswer = prior, "call_0", "yes"
		res, err := budgetBackend(client, 1).Execute(budgetCtx(), task)
		requireVerdict(t, res, err)
		if got := ran.Load(); got != 5 {
			t.Fatalf("the resume ran %d tool turns, want the whole bound 5 (requests %s)", got, requestKinds(client))
		}
	})
}

// A compaction over turns that reuse a call id records each folded turn,
// the reused key as often as it was folded: the count of a conversation's
// turns survives the fold.
//
// Mutant: record the keys the compaction no longer shows by identity, and
// every folded turn whose key a kept turn reuses vanishes from the count.
func TestACompactionRecordsEveryFoldedTurnOfAReusedID(t *testing.T) {
	t.Parallel()
	conversation := sizedStoredRounds(0, 0)
	for range 8 {
		conversation = append(conversation,
			api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "call_0", Name: "probe", Input: map[string]any{}}}},
			api.Message{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: "call_0", Content: strings.Repeat("observed ", 400)}.ToContentBlock()}})
	}
	res := compactMessagesToolSafe(conversation, clawrt.CompactionConfig{PreserveRecentMessages: 4, MaxEstimatedTokens: 1}, nil)
	if res == nil || len(res.CompactedMessages) >= len(conversation) {
		t.Fatal("scenario broken: nothing was compacted")
	}
	if got := completedToolRounds(res.CompactedMessages); got != 8 {
		t.Fatalf("the compacted conversation counts %d completed tool turns, want the 8 it held", got)
	}
}

// A turn whose call executed nothing — an unknown tool, answered with an
// error — grounds nothing: a retry that replays only that turn keeps an
// always-thinking model's contract to run a tool before it answers, as
// its first attempt would.
//
// Mutant: lift the force on any completed turn of this execution, and the
// retry answers without a tool ever having run.
func TestClawStepBudget_ATurnThatRanNoToolLiftsNothing(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
		switch n {
		case 1:
			return toolUseEvents("tu_nope", "no_such_tool", `{}`, 10, 5), nil
		case 2:
			return cutStream(), nil
		}
		return textEvents("The diff is fine.", 10, 5), nil
	}}
	task := budgetTask(probe, 5)
	task.OutputSchema, task.Model = nil, "test/claude-opus-5-5"
	_, err := budgetBackend(client, 2).Execute(budgetCtx(), task)
	if err == nil || !strings.Contains(err.Error(), "returned before executing a required initial tool") {
		t.Fatalf("err = %v, want the contract's refusal: no tool ran (%d probe runs)", err, ran.Load())
	}
}

// hasFoldRecord reports whether a conversation carries a compaction's fold
// record.
func hasFoldRecord(conversation []api.Message) bool {
	return slices.ContainsFunc(conversation, func(m api.Message) bool {
		return slices.ContainsFunc(m.Content, func(b api.ContentBlock) bool {
			return b.Type == "text" && strings.HasPrefix(b.Text, foldedToolTurnsMarker)
		})
	})
}

// A persisted session outlives the node execution whose tool steps its fold
// records count, so it carries none, written or read: a slot conversation
// compacted visit after visit does not re-record every turn it ever folded.
//
// Mutants: keep the record a persisted conversation arrives with, and the
// visits and the inbound envelope carry it; keep the record the replay
// clamp's own compaction writes, and the oversized replay carries it.
func TestAPersistedSessionCarriesNoFoldRecord(t *testing.T) {
	t.Parallel()
	const runID, slot, sessionID = "run-persist", "assistant_conversation", "session-1"
	// The subtests share one seeded source, so they run in turn.
	r := rand.New(rand.NewSource(3283))
	turns := func(visit, n, size int) []api.Message {
		var out []api.Message
		for i := range n {
			buf := make([]byte, size)
			r.Read(buf)
			id := fmt.Sprintf("tu_%d_%d", visit, i)
			out = append(out,
				api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: id, Name: "probe", Input: map[string]any{}}}},
				api.Message{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: id, Content: hex.EncodeToString(buf)}.ToContentBlock()}})
		}
		return out
	}
	t.Run("visits of a slot", func(t *testing.T) {
		conversation := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Help me with the repo."}}}}
		for visit := 1; visit <= 4; visit++ {
			conversation = append(conversation, turns(visit, 12, 3000)...)
			// The visit's own loop compacts, as a long loop does.
			compacted, ok := forceCompactToTokens(conversation, 8000, 4)
			if !ok || !hasFoldRecord(compacted) {
				t.Fatalf("visit %d: scenario broken, the loop's compaction recorded nothing", visit)
			}
			source := &ClawExecutor{sessions: newNodeSessionStore()}
			source.sessions.save(runID, slot, compacted)
			blob := source.packClawSession(runID, slot, sessionID, "claw:anthropic")
			var envelope clawPersistEnvelope
			if err := json.Unmarshal(blob, &envelope); err != nil {
				t.Fatalf("visit %d: decode the packed envelope: %v", visit, err)
			}
			if hasFoldRecord(envelope.Messages) {
				t.Fatalf("visit %d: the persisted session carries a fold record", visit)
			}
			target := &ClawExecutor{sessions: newNodeSessionStore()}
			if err := target.UnpackSession(WithRunID(context.Background(), runID), "claw", sessionID, blob); err != nil {
				t.Fatalf("visit %d: unpack: %v", visit, err)
			}
			conversation = target.sessions.load(runID, slot)
		}
	})
	t.Run("an oversized replay", func(t *testing.T) {
		conversation := append([]api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Inspect the workflow."}}}}, turns(0, 3, 30000)...)
		source := &ClawExecutor{sessions: newNodeSessionStore()}
		source.sessions.save(runID, slot, conversation)
		blob := source.packClawSession(runID, slot, sessionID, "claw:anthropic")
		var envelope clawPersistEnvelope
		if err := json.Unmarshal(blob, &envelope); err != nil {
			t.Fatalf("decode the packed envelope: %v", err)
		}
		if len(envelope.Messages) >= len(conversation) {
			t.Fatalf("scenario broken: the replay clamp compacted nothing (%d messages)", len(envelope.Messages))
		}
		if hasFoldRecord(envelope.Messages) {
			t.Fatal("the clamped replay carries the fold record of its own compaction")
		}
	})
	t.Run("an inbound envelope", func(t *testing.T) {
		compacted, ok := forceCompactToTokens(append([]api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Help me."}}}}, turns(9, 12, 3000)...), 8000, 4)
		if !ok || !hasFoldRecord(compacted) {
			t.Fatal("scenario broken: no fold record to read")
		}
		blob, err := marshalClawPersistEnvelope(slot, sessionID, "claw:anthropic", compacted)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		target := &ClawExecutor{sessions: newNodeSessionStore()}
		if err := target.UnpackSession(WithRunID(context.Background(), runID), "claw", sessionID, blob); err != nil {
			t.Fatalf("unpack: %v", err)
		}
		if got := target.sessions.load(runID, slot); len(got) == 0 || hasFoldRecord(got) {
			t.Fatalf("the restored session is empty or carries a fold record (%d messages)", len(got))
		}
	})
}

// A retry of an execution that started from a session already holding
// turns under the call id the provider reuses: the anchor names the
// occurrence of that id the execution started at, so the retry — which
// replays 3 earlier turns and the execution's own 2, all under one id —
// is charged the 2 and runs the 3 the bound leaves.
//
// Mutant: resolve the anchor at the LAST occurrence of its key, and the
// retry is charged nothing: 7 for 5.
func TestClawStepBudget_ARetryOverAPriorSessionsReusedIDsIsChargedItsOwnTurns(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"call_0", "functions.probe:0"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			probe, ran := budgetProbe()
			client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				if n == 3 {
					return cutStream(), nil
				}
				if offersTool(req, "probe") {
					return toolUseEvents(id, "probe", `{}`, 10, 5), nil
				}
				return toolUseEvents("so_1", "structured_output", `{"verdict":"done"}`, 10, 5), nil
			}}
			store := newNodeSessionStore()
			store.save("run-budget", "judge", storedRoundsWithIDs(slices.Repeat([]string{id}, 3)...))
			res, err := budgetBackend(client, 2).Execute(withRuntimeContext(context.Background(), "run-budget", store), budgetTask(probe, 5))
			requireVerdict(t, res, err)
			if got := ran.Load(); got != 5 {
				t.Fatalf("%d tool turns ran in one execution for tool_max_steps 5 (requests %s)", got, requestKinds(client))
			}
		})
	}
}

// An execution that starts from a session holding tools that ran — a
// persisted slot revisited, as it was or as a compaction summarised it —
// has grounded nothing of its own: when its only turn is a call answered
// with an error and a cut retries it, an always-thinking model's contract
// to run a tool before it answers still holds.
//
// Mutants: count the anchor turn's own result as after it, or scan every
// result from the start (the earlier session's tools lift the force);
// count the turns without those a summary folded (the anchor is never
// found, every result counts).
func TestClawStepBudget_APriorSessionsToolsLiftNothing(t *testing.T) {
	t.Parallel()
	summarised := func() []api.Message {
		compacted := compactMessagesToolSafe(storedRoundsWithIDs("tu_prior_1", "tu_prior_2", "tu_prior_3", "tu_prior_4", "tu_prior_5", "tu_prior_6"),
			clawrt.CompactionConfig{PreserveRecentMessages: 4, MaxEstimatedTokens: 1}, nil)
		if compacted == nil || !hasFoldRecord(compacted.CompactedMessages) || completedToolRounds(compacted.CompactedMessages) != 6 {
			t.Fatal("scenario broken: the earlier session is not summarised with its 6 turns recorded")
		}
		return compacted.CompactedMessages
	}
	for _, tc := range []struct {
		name  string
		prior func() []api.Message
	}{
		{name: "the session as it was", prior: func() []api.Message { return storedRoundsWithIDs("tu_prior_1", "tu_prior_2") }},
		{name: "the session a compaction summarised", prior: summarised},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe, ran := budgetProbe()
			client := &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				switch n {
				case 1:
					return toolUseEvents("tu_nope", "no_such_tool", `{}`, 10, 5), nil
				case 2:
					return cutStream(), nil
				}
				return textEvents("The diff is fine.", 10, 5), nil
			}}
			task := budgetTask(probe, 5)
			task.OutputSchema, task.Model = nil, "test/claude-opus-5-5"
			store := newNodeSessionStore()
			store.save("run-budget", "judge", tc.prior())
			_, err := budgetBackend(client, 2).Execute(withRuntimeContext(context.Background(), "run-budget", store), task)
			if err == nil || !strings.Contains(err.Error(), "returned before executing a required initial tool") {
				t.Fatalf("err = %v, want the contract's refusal: no tool of this execution ran (%d probe runs)", err, ran.Load())
			}
		})
	}
}
