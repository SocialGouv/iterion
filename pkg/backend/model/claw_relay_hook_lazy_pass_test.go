package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// countingEmitter is the store sink reduced to what these hooks need: the
// event copy is Layer 0's business, not this witness's.
type countingEmitter struct{ events int }

func (e *countingEmitter) AppendEvent(context.Context, string, store.Event) (*store.Event, error) {
	e.events++
	return nil, nil
}

// Ask the sink before the pass — at the relayed-hook sinks too, not only at
// the three in claw_backend.go.
//
// Every far-side value these hooks write to the operator's log costs the run's
// secret guard (an unmaterialize, a heuristic detector and a recursive decode
// over the whole text), and the logger drops a record above its level before
// it formats the line. A relayed retry's error is bounded only by the IPC's
// per-field budget — 1 MiB — so a run at `--log-level error` would pay the
// whole pass for a line it discards. Each site is gated on its own level, so
// each subtest reddens alone; the KEEPS arm proves the drive reaches the site.
//
// A formatted line is lazy by itself; a block BODY is a string the caller
// builds, so the block sinks pay through [storeHooks.logFarSideBlock] — one
// owner of the gate for all of them, and one subtest per body it carries.
func TestClawRelay_AHookLogThatDropsTheRecordCostsNoPass(t *testing.T) {
	const secret = "sk-proj-WITNESSHOOKLAZY0123456789abcdef"
	bulky := strings.Repeat("x", 4096) + secret

	for _, tc := range []struct {
		name         string
		drops, keeps iterlog.Level
		drive        func(h *storeHooks)
	}{
		{
			name:  "a relayed retry's error, reported at warn",
			drops: iterlog.LevelError,
			keeps: iterlog.LevelWarn,
			drive: func(h *storeHooks) {
				h.onLLMRetry("campaign", RetryInfo{Attempt: 2, Error: errors.New(bulky)})
			},
		},
		{
			name:  "a relayed call's model and reasoning effort, reported at info",
			drops: iterlog.LevelWarn,
			keeps: iterlog.LevelInfo,
			drive: func(h *storeHooks) {
				h.onLLMRequest("campaign", LLMRequestInfo{Model: "openai/" + secret, ReasoningEffort: secret, MessageCount: 2})
			},
		},
		{
			name:  "a relayed step's tool names and inputs, reported at info",
			drops: iterlog.LevelWarn,
			keeps: iterlog.LevelInfo,
			drive: func(h *storeHooks) {
				h.onLLMStepFinish("campaign", LLMStepInfo{
					Number: 1,
					ToolCalls: []ToolCallEntry{{
						Name:  "write_file",
						Input: json.RawMessage(`{"path":"a.txt","content":"` + secret + `"}`),
					}},
				})
			},
		},
		// The same function's two BLOCK bodies. What the gate saves is not
		// the same at the two: the thinking text is deliberately not
		// persisted at all, so the block's pass is the ONLY one over it,
		// while the response text also travels in the step event, whose copy
		// Layer 0 redacts whatever the log level is (data["response_text"]
		// is built unconditionally). A dropped record therefore removes the
		// only pass over a relayed step's thinking, and one of the two over
		// its response. What this witness counts is the sink's own pass: its
		// emitter is a counting stub, which pays none.
		{
			name:  "a relayed step's response text, reported at info",
			drops: iterlog.LevelWarn,
			keeps: iterlog.LevelInfo,
			drive: func(h *storeHooks) {
				h.onLLMStepFinish("campaign", LLMStepInfo{Number: 1, Text: bulky})
			},
		},
		{
			name:  "a relayed step's thinking text, reported at info",
			drops: iterlog.LevelWarn,
			keeps: iterlog.LevelInfo,
			drive: func(h *storeHooks) {
				h.onLLMStepFinish("campaign", LLMStepInfo{Number: 1, Thinking: bulky, ReasoningTokens: 10})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive := func(level iterlog.Level) int {
				var passes int
				var logs strings.Builder
				h := &storeHooks{
					ctx:     context.Background(),
					emitter: &countingEmitter{},
					runID:   "run-hook-lazy",
					logger:  iterlog.New(level, &logs),
					red: func(s string) string {
						passes++
						return strings.ReplaceAll(s, secret, "__ITERION_SECRET_openai_key__")
					},
				}
				tc.drive(h)
				return passes
			}
			if kept := drive(tc.keeps); kept == 0 {
				t.Fatalf("the sink that KEEPS this line asked for no pass — this witness no longer reaches the site it watches")
			}
			if dropped := drive(tc.drops); dropped != 0 {
				t.Errorf("a run log that drops the record still paid %d pass(es) over the far side's text", dropped)
			}
		})
	}
}

// The gate drops no record the log keeps.
//
// [storeHooks.logFarSideBlock] asks the sink and then redacts, and nothing
// else: whether an empty body is worth a header stays the caller's call, as it
// is for [iterlog.Logger.LogBlock] itself. A helper short-circuiting on an
// empty body reads as harmless and silently drops the raw-input record of
// every tool call made with no arguments — the record an operator at
// --log-level debug is reading the raw input FOR.
func TestClawRelay_TheBlockGateDropsNoRecordTheLogKeeps(t *testing.T) {
	for _, input := range []json.RawMessage{json.RawMessage(""), json.RawMessage(`{"path":"a.txt"}`)} {
		t.Run("input="+string(input), func(t *testing.T) {
			at := func(level iterlog.Level) string {
				var logs strings.Builder
				h := &storeHooks{
					ctx:     context.Background(),
					emitter: &countingEmitter{},
					runID:   "run-hook-block-gate",
					logger:  iterlog.New(level, &logs),
					red:     func(s string) string { return s },
				}
				h.onLLMStepFinish("n", LLMStepInfo{Number: 1, ToolCalls: []ToolCallEntry{{Name: "noop_tool", Input: input}}})
				return logs.String()
			}
			if got := at(iterlog.LevelDebug); !strings.Contains(got, "raw input noop_tool:") {
				t.Errorf("a run log at --log-level debug lost the raw-input record: %q", got)
			}
			if got := at(iterlog.LevelInfo); strings.Contains(got, "raw input noop_tool:") {
				t.Errorf("a run log at --log-level info was given a DEBUG record: %q", got)
			}
		})
	}
}
