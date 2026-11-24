package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The claw tool lines CUT the raw model/tool text into their log headers
// before the run's pass could cross it: the delegate bridge's hint keeps 57
// bytes, the result preview 100, and the in-process claw path's header
// detail 100. A declared value straddling the cut published its head while
// the body below showed the placeholder — the head is what no matcher
// recognises. The pass crosses the text BEFORE each cut (iterion#2822),
// with the raw payload kept for the event sink (the redacting emitter
// scrubs it there).
//
// clawDisplayWire builds the production bridge shape: the store event hooks
// and the delegate TaskHooks on one guard, the logger wired as
// BuildExecutor wires the run's, with the guard's pass installed.
func clawDisplayWire(t *testing.T) (delegate.TaskHooks, *strings.Builder, string, string) {
	t.Helper()
	const value = "hunter2-9f8e7d6c5b4a-db-password"
	placeholder := secretguard.PlaceholderForName("db")
	g := secretguard.New([]secretguard.Secret{
		{Name: "db", Value: value, Placeholder: placeholder},
	}, secretguard.DefaultConfig())
	var buf strings.Builder
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	logger.SetRedactor(g.Redact)
	em := &captureEmitter{}
	hooks := NewStoreEventHooks(context.Background(), em, "run-1", logger, g, nil)
	e := newFallbackExecutor(delegate.NewRegistry(), hooks)
	e.secretGuard = g
	e.logger = logger
	return e.delegateHooksFor("n1", delegate.BackendClaw, 1), &buf, value, placeholder
}

// headSurviving returns the value's head a truncate(s, n) keeps: n-3 bytes
// past the offset (the "..." suffix eats three).
func headSurviving(value string, at, n int) string {
	return value[:min(n-3-at, len(value))]
}

func TestTheClawToolLinesRedactBeforeTheDisplayCuts(t *testing.T) {
	t.Parallel()
	t.Run("tool_use_hint_cuts_raw_input", func(t *testing.T) {
		th, buf, value, placeholder := clawDisplayWire(t)

		cmd := "cd /srv/app && ./deploy.sh --token " + value + " --env prod"
		at := strings.Index(cmd, value)
		if at < 0 || at >= 57 || at+len(value) <= 60 {
			t.Fatalf("the premise of this test is wrong: the value must straddle the 57-byte hint cut (at=%d)", at)
		}
		head := headSurviving(value, at, 57)

		th.OnToolStarted("Bash", "tu1", json.RawMessage(`{"command":`+mustJSONString(cmd)+`}`))

		got := buf.String()
		if !strings.Contains(got, "[n1#1/claw] 🔧 Bash") {
			t.Fatalf("the tool line never reached the log, so this test proves nothing: %q", got)
		}
		if strings.Contains(got, head) {
			t.Errorf("the tool-use hint published the value's head %q — the hint cut before the pass: %q", head, got)
		}
		if !strings.Contains(got, placeholder) {
			t.Errorf("the tool line shows no placeholder where the value was: %q", got)
		}
	})

	t.Run("result_display_cuts_raw_output", func(t *testing.T) {
		th, buf, value, placeholder := clawDisplayWire(t)

		out := "step ok; uploaded to https://internal.example.com/redactor/ingest?id=abcdefghij; token=" + value + " done"
		at := strings.Index(out, value)
		if at < 0 || at >= 97 || at+len(value) <= 100 {
			t.Fatalf("the premise of this test is wrong: the value must straddle the 100-byte result header cut (at=%d)", at)
		}
		head := headSurviving(value, at, 100)

		th.OnToolCalled("Bash", "tu2", false, out)

		got := buf.String()
		if !strings.Contains(got, "[n1#1/claw] 📤 Bash") {
			t.Fatalf("the result line never reached the log, so this test proves nothing: %q", got)
		}
		if strings.Contains(got, head) {
			t.Errorf("the result header published the value's head %q — the display cut ran before the pass: %q", head, got)
		}
		if !strings.Contains(got, placeholder) {
			t.Errorf("the result line shows no placeholder where the value was: %q", got)
		}
	})
}

// The in-process claw path renders each tool call through
// tooldisplay.HeaderDetail, which keeps 100 bytes: the run's pass crosses
// the raw IPC input BEFORE the cut, or a value straddling it publishes its
// head. Driven through the real store hooks, on the logger BuildExecutor
// wires the run's.
func TestTheInProcessClawHeaderRedactsBeforeItCuts(t *testing.T) {
	t.Parallel()
	const value = "hunter2-9f8e7d6c5b4a-db-password"
	placeholder := secretguard.PlaceholderForName("db")
	g := secretguard.New([]secretguard.Secret{
		{Name: "db", Value: value, Placeholder: placeholder},
	}, secretguard.DefaultConfig())
	var buf strings.Builder
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	logger.SetRedactor(g.Redact)
	h := &storeHooks{
		ctx:     context.Background(),
		emitter: &captureEmitter{},
		runID:   "run-1",
		logger:  logger,
		red:     g.Redact,
	}

	cmd := "cd /srv/app && ./deploy.sh --token " + value + " --env prod"
	cmd = "cd /srv/app" + strings.Repeat(" && ./step.sh", 4)[:50] + cmd[len("cd /srv/app"):]
	at := strings.Index(cmd, value)
	if at < 0 || at >= 97 || at+len(value) <= 100 {
		t.Fatalf("the premise of this test is wrong: the value must straddle the 100-byte header cut (at=%d)", at)
	}
	head := headSurviving(value, at, 100)

	h.onLLMStepFinish("n1", LLMStepInfo{Number: 1, Iteration: 1, ToolCalls: []ToolCallEntry{
		{Name: "Bash", Input: json.RawMessage(`{"command":` + mustJSONString(cmd) + `}`)},
	}})

	got := buf.String()
	if !strings.Contains(got, "[n1#1/claw] Bash") {
		t.Fatalf("the in-process tool line never reached the log, so this test proves nothing: %q", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("the in-process header published the value's head %q — the display cut ran before the pass: %q", head, got)
	}
	if !strings.Contains(got, placeholder) {
		t.Errorf("the in-process header shows no placeholder where the value was: %q", got)
	}
}

// mustJSONString marshals s as a JSON string literal for embedding in a
// RawMessage under test.
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
