package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestFormattingPassVerdicts drives the formatting loop through the test seam:
// a terminal verdict returns at once with the billed pass counted and its
// cost stamped; a throttle is retried and the second attempt's answer ships
// with both passes' usage; a transient render exhausts the attempts typed;
// the recovery pass returns its message with the verdict.
func TestFormattingPassVerdicts(t *testing.T) {
	prevDelay := formatRetryDelay
	formatRetryDelay = 0
	t.Cleanup(func() { formatRetryDelay = prevDelay })
	str := func(s string) *string { return &s }
	f := func(v float64) *float64 { return &v }
	schema := json.RawMessage(`{"type":"object","required":["answer","count"],"properties":{"answer":{"type":"string"},"count":{"type":"integer"}}}`)
	task := Task{NodeID: "n", Iteration: 1, Model: "claude-fable-5", OutputSchema: schema}
	pass1 := &claudesdk.ResultMessage{Result: str("I looked at the code and here is my free-form conclusion."), SessionID: "s1",
		Usage: &claudesdk.Usage{InputTokens: 100, OutputTokens: 10}, TotalCostUSD: f(0.10)}
	newBackend := func(replies ...*claudesdk.ResultMessage) (*ClaudeCodeBackend, *int) {
		calls := 0
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
		b.formatOutputFn = func(context.Context, Task, string) (*claudesdk.ResultMessage, error) {
			i := calls
			calls++
			if i >= len(replies) {
				t.Fatalf("formatting pass called %d times, only %d replies scripted", calls, len(replies))
			}
			return replies[i], nil
		}
		return b, &calls
	}
	render := func(text string, cost float64) *claudesdk.ResultMessage {
		return &claudesdk.ResultMessage{Result: str(text), SessionID: "s1",
			Usage: &claudesdk.Usage{InputTokens: 50, OutputTokens: 5}, TotalCostUSD: f(cost)}
	}

	t.Run("a credential verdict ends the loop after one billed pass, cost stamped", func(t *testing.T) {
		b, calls := newBackend(render("API Error: [401][Unauthorized][abc]", 0.25))
		in, out := 100, 10
		handled, res, err := b.runTwoPassFormatting(context.Background(), task, pass1, Result{Tokens: 110}, &in, &out)
		var auth *ErrAuthFailed
		if !handled || !errors.As(err, &auth) {
			t.Fatalf("want handled + ErrAuthFailed, got handled=%v err=%v", handled, err)
		}
		if *calls != 1 {
			t.Fatalf("formatting pass called %d times, want 1", *calls)
		}
		if res.Tokens != 165 || !res.FormattingPassUsed {
			t.Fatalf("billed pass not counted: tokens=%d used=%v", res.Tokens, res.FormattingPassUsed)
		}
		if res.Output == nil || res.Output["_cost_usd"] == nil || res.Output["_tokens"] == nil {
			t.Fatalf("cost not stamped on the terminal path: %v", res.Output)
		}
	})
	t.Run("a throttle is retried; the second attempt's answer ships with both passes counted", func(t *testing.T) {
		answer := &claudesdk.ResultMessage{Result: str(`{"answer":"done","count":2}`), SessionID: "s1",
			Usage: &claudesdk.Usage{InputTokens: 40, OutputTokens: 4}, TotalCostUSD: f(0.30)}
		b, calls := newBackend(render("API Error: 429 rate limit exceeded", 0.20), answer)
		in, out := 100, 10
		handled, res, err := b.runTwoPassFormatting(context.Background(), task, pass1, Result{Tokens: 110}, &in, &out)
		if !handled || err != nil {
			t.Fatalf("want a shipped answer, got handled=%v err=%v", handled, err)
		}
		if *calls != 2 {
			t.Fatalf("formatting pass called %d times, want 2", *calls)
		}
		if res.Output["answer"] != "done" || res.ParseFallback {
			t.Fatalf("answer not shipped: %v fallback=%v", res.Output, res.ParseFallback)
		}
		if res.Tokens != 110+55+44 {
			t.Fatalf("both passes must count: tokens=%d", res.Tokens)
		}
	})
	t.Run("a transient render exhausts the attempts, typed", func(t *testing.T) {
		b, calls := newBackend(render("API Error: [500][Operation failed][x]", 0.2), render("API Error: [500][Operation failed][y]", 0.3))
		in, out := 100, 10
		handled, res, err := b.runTwoPassFormatting(context.Background(), task, pass1, Result{Tokens: 110}, &in, &out)
		var tr *ErrTransient
		if !handled || !errors.As(err, &tr) {
			t.Fatalf("want handled + ErrTransient, got handled=%v err=%v", handled, err)
		}
		if *calls != 2 || res.Tokens != 220 || res.Output["_cost_usd"] == nil {
			t.Fatalf("exhausted path: calls=%d tokens=%d output=%v", *calls, res.Tokens, res.Output)
		}
	})
	t.Run("the recovery pass returns its billed message with the verdict", func(t *testing.T) {
		b, calls := newBackend(render("API Error: [429][Usage limit reached for 5 hour. Your limit will reset at 3pm][abc]", 0.4))
		in, out := 100, 10
		res := Result{Tokens: 110}
		fmtRM, err := b.runRecoveryFormatterPass(context.Background(), task, "s1", &res, &in, &out)
		var rl *ErrRateLimited
		if !errors.As(err, &rl) || fmtRM == nil {
			t.Fatalf("want the message with ErrRateLimited, got rm=%v err=%v", fmtRM, err)
		}
		if *calls != 1 || res.Tokens != 165 || !res.FormattingPassUsed {
			t.Fatalf("billed recovery pass not counted: calls=%d tokens=%d", *calls, res.Tokens)
		}
	})
	t.Run("a pass that could not run still prices the delegation on the exhausted path", func(t *testing.T) {
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
		calls := 0
		b.formatOutputFn = func(context.Context, Task, string) (*claudesdk.ResultMessage, error) {
			calls++
			return nil, errors.New("container is not running")
		}
		in, out := 100, 10
		handled, res, err := b.runTwoPassFormatting(context.Background(), task, pass1, Result{Tokens: 110}, &in, &out)
		if !handled || err == nil || calls != 2 {
			t.Fatalf("want handled + error after 2 attempts, got handled=%v err=%v calls=%d", handled, err, calls)
		}
		if res.Output == nil || res.Output["_cost_usd"] == nil {
			t.Fatalf("Pass 1's cost dropped when the last attempt produced no message: %v", res.Output)
		}
	})
	t.Run("a typed failure carries the delegation's spend", func(t *testing.T) {
		res := Result{}
		err := typedFailure(&res, task, 100, 10, errors.New("x"), pass1)
		if err == nil || res.Output == nil || res.Output["_cost_usd"] == nil || res.Output["_tokens"] == nil {
			t.Fatalf("typed failure without its spend: err=%v output=%v", err, res.Output)
		}
	})
	t.Run("an SDK object beside a render is not an answer; beside plain prose it is", func(t *testing.T) {
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
		obj := map[string]any{"answer": "done", "count": 1}
		rm := &claudesdk.ResultMessage{Result: str("API Error: [429][Usage limit reached for 5 hour. Your limit will reset at 3pm][abc]"), StructuredOutput: obj}
		var rl *ErrRateLimited
		if err := b.renderedFailure(rm, task, "formatting pass 1/2"); !errors.As(err, &rl) {
			t.Fatalf("window verdict shipped as an answer beside an echoed object: %v", err)
		}
		rm = &claudesdk.ResultMessage{Result: str("Claude AI usage limit reached|1757200000"), StructuredOutput: obj}
		if err := b.renderedFailure(rm, task, "formatting pass 1/2"); !errors.As(err, &rl) {
			t.Fatalf("non-bracketed refusal shipped as an answer beside an object: %v", err)
		}
		rm = &claudesdk.ResultMessage{Result: str("Done: the report lists the quota policy and the two remaining lots."), StructuredOutput: obj}
		if err := b.renderedFailure(rm, task, "formatting pass 1/2"); err != nil {
			t.Fatalf("an object beside plain prose re-typed: %v", err)
		}
	})
	t.Run("a raw error body is not an answer", func(t *testing.T) {
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
		rm := &claudesdk.ResultMessage{Result: str(`{"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`)}
		if err := b.renderedFailure(rm, task, "pass 1"); err == nil {
			t.Fatalf("a raw error body shipped as an answer")
		}
	})
	t.Run("a render carrying a fenced JSON is still a render", func(t *testing.T) {
		b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
		rm := &claudesdk.ResultMessage{Result: str("API Error: 503 ```json\n{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n```")}
		var tr *ErrTransient
		if err := b.renderedFailure(rm, task, "pass 1"); !errors.As(err, &tr) {
			t.Fatalf("render with a fenced body exempted as an answer: %v", err)
		}
	})
}
