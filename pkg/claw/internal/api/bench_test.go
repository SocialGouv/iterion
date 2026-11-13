package api_test

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/hooks"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/httputil"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/providers/openaiwire"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/sseutil"
	"github.com/SocialGouv/iterion/pkg/claw/internal/permissions"
)

// BenchmarkSseutilAccumulator_HandleDelta measures the per-fragment overhead
// of pushing argument deltas through a single ToolCallAccumulator that has
// already been MarkStarted. This is the inner loop of every streaming
// tool_use translation in the openai/foundry/responses providers.
func BenchmarkSseutilAccumulator_HandleDelta(b *testing.B) {
	acc := sseutil.NewToolCallAccumulator(0)
	_ = acc.MarkStarted("id1", "name1")
	frag := `{"k":"v"}`

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Inner micro-loop of 1000 fragments per b.N to amortise function-call
		// overhead and keep the wall-clock per-op meaningful even for very
		// fast paths.
		for j := 0; j < 1000; j++ {
			_ = acc.HandleDelta("", "", frag)
		}
	}
}

// BenchmarkOpenaiwire_ConvertTools_Small measures ConvertTools on a tiny tool
// set typical of single-purpose agents (5 tools, flat schemas).
func BenchmarkOpenaiwire_ConvertTools_Small(b *testing.B) {
	tools := makeTools(5, false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := openaiwire.ConvertTools("bench", tools)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpenaiwire_ConvertTools_Large measures ConvertTools on a 50-tool
// set with nested object properties — closer to a fully-loaded code agent.
func BenchmarkOpenaiwire_ConvertTools_Large(b *testing.B) {
	tools := makeTools(50, true)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := openaiwire.ConvertTools("bench", tools)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpenaiwire_ConvertMessages measures ConvertMessages on a 30-turn
// conversation alternating user/assistant with mixed text and tool_use blocks.
func BenchmarkOpenaiwire_ConvertMessages(b *testing.B) {
	messages := makeConversation(30)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = openaiwire.ConvertMessages("you are a helpful assistant", messages)
	}
}

// BenchmarkHttputil_TruncateBody_Short exercises the no-op branch of
// TruncateBody: input shorter than the budget returns unchanged.
func BenchmarkHttputil_TruncateBody_Short(b *testing.B) {
	body := "1234567890" // 10 chars

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = httputil.TruncateBody(body, 200)
	}
}

// BenchmarkHttputil_TruncateBody_Long exercises the rune-slice + ellipsis
// branch of TruncateBody on a 10kB body truncated to 1000.
func BenchmarkHttputil_TruncateBody_Long(b *testing.B) {
	body := strings.Repeat("a", 10000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = httputil.TruncateBody(body, 1000)
	}
}

// BenchmarkHooksRunner_Fire_NilHandlers measures the fast-path return when
// no hook commands are configured — the zero-config case every tool call
// hits in the common case.
func BenchmarkHooksRunner_Fire_NilHandlers(b *testing.B) {
	runner := hooks.NewHookRunner(hooks.HookConfig{})
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runner.RunPreToolUse(ctx, "read_file", `{"path":"/tmp/x"}`)
	}
}

// BenchmarkHooksRunner_Fire_3Handlers measures dispatch through a
// 3-command chain. The handlers execute the POSIX `true` builtin so the
// per-op cost is dominated by process spawn, not hook bookkeeping. Useful
// to track regressions in env/payload assembly and the per-command loop.
func BenchmarkHooksRunner_Fire_3Handlers(b *testing.B) {
	testutil.IsolateShellHome(b)
	runner := hooks.NewHookRunner(hooks.HookConfig{
		PreToolUse: []string{"true", "true", "true"},
	})
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runner.RunPreToolUse(ctx, "read_file", `{"path":"/tmp/x"}`)
	}
}

// BenchmarkPermissionsClassifier_DefaultRule measures the default-safe-list
// RuleClassifier hot path: tool name is in the allow-list so it returns
// DecisionAllow on first lookup. Run as a 1000-call inner loop per b.N to
// keep per-op timings stable.
func BenchmarkPermissionsClassifier_DefaultRule(b *testing.B) {
	rc := permissions.NewRuleClassifier()
	ctx := context.Background()
	args := map[string]any{"path": "/tmp/x"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			_, _ = rc.Classify(ctx, "read_file", args)
		}
	}
}

// makeTools returns n synthetic api.Tool definitions. When nested is true
// each tool gets an object-typed property with three nested string
// properties to simulate moderate-complexity schemas.
func makeTools(n int, nested bool) []api.Tool {
	tools := make([]api.Tool, n)
	for i := 0; i < n; i++ {
		props := map[string]api.Property{
			"path":  {Type: "string", Description: "absolute file path"},
			"limit": {Type: "integer", Description: "max items"},
		}
		if nested {
			props["filter"] = api.Property{
				Type: "object",
				Properties: map[string]api.Property{
					"include":  {Type: "string"},
					"exclude":  {Type: "string"},
					"glob_set": {Type: "array", Items: &api.Property{Type: "string"}},
				},
				Required: []string{"include"},
			}
		}
		tools[i] = api.Tool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: "synthetic benchmark tool",
			InputSchema: api.InputSchema{
				Type:       "object",
				Properties: props,
				Required:   []string{"path"},
			},
		}
	}
	return tools
}

// makeConversation returns a 2*turns-message conversation alternating user
// (text + tool_result) and assistant (text + tool_use) blocks.
func makeConversation(turns int) []api.Message {
	msgs := make([]api.Message, 0, 2*turns)
	for i := 0; i < turns; i++ {
		msgs = append(msgs, api.Message{
			Role: "user",
			Content: []api.ContentBlock{
				{Type: "text", Text: fmt.Sprintf("turn %d question", i)},
				{
					Type:      "tool_result",
					ToolUseID: fmt.Sprintf("call_%d", i),
					Content:   []api.ContentBlock{{Type: "text", Text: "ok"}},
				},
			},
		})
		msgs = append(msgs, api.Message{
			Role: "assistant",
			Content: []api.ContentBlock{
				{Type: "text", Text: fmt.Sprintf("turn %d answer", i)},
				{
					Type:  "tool_use",
					ID:    fmt.Sprintf("call_%d", i+1),
					Name:  "read_file",
					Input: map[string]any{"path": "/tmp/x"},
				},
			},
		})
	}
	return msgs
}
