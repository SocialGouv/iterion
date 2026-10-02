package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A tool node is where a workflow's deterministic gates live, and its
// completion event is what an operator reads when one fails. Carrying only
// `exit status 1` there leaves a verdict nobody can act on: the failing
// command's own diagnosis exists — it went to the run log — but the event
// the failure is audited from never had it.

// captureToolCall runs one direct tool node and returns what the two finish
// hooks were handed. Both are recorded, because the invariant is that the
// event and the log carry the SAME string: one shape, two sinks.
func captureToolCall(t *testing.T, node *ir.ToolNode) (LLMToolCallInfo, string, error) {
	t.Helper()
	var info LLMToolCallInfo
	var logged string
	var sawCall, sawResult bool
	exec := newTestClawExecutor(NewRegistry(), &ir.Workflow{},
		WithToolPolicy(tool.OpenPolicy()),
		WithEventHooks(EventHooks{
			OnToolCall: func(_ string, got LLMToolCallInfo) {
				info, sawCall = got, true
			},
			OnToolNodeResult: func(_, _ string, _ []byte, output string, _ time.Duration, _ error) {
				logged, sawResult = output, true
			},
		}),
	)
	_, err := exec.Execute(context.Background(), node, map[string]any{})
	if !sawCall {
		t.Fatal("OnToolCall never fired for a tool node that ran")
	}
	if !sawResult {
		t.Fatal("OnToolNodeResult never fired for a tool node that ran")
	}
	return info, logged, err
}

func TestFailingToolNodeCarriesItsOutputOnTheEvent(t *testing.T) {
	// Both streams: a gate prints its verdict on stdout and its diagnosis
	// on stderr, and reading one without the other is how an environment
	// failure gets mistaken for a red verdict.
	const body = "echo VERDICT-LINE; echo DIAGNOSIS-LINE 1>&2; exit 1"

	cases := []struct {
		name string
		node *ir.ToolNode
	}{
		{"script", &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "gate"},
			Language: "sh",
			Script:   body,
		}},
		{"shell", &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "gate"},
			Command:  body,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, logged, err := captureToolCall(t, tc.node)
			if err == nil {
				t.Fatal("Execute returned nil error for a node that exited 1")
			}
			if info.Error == nil {
				t.Fatal("the event carries no error for a node that exited 1")
			}
			// The precise shape the defect showed up as: the whole event
			// was this one string, and nothing else.
			if info.Error.Error() != "exit status 1" {
				t.Errorf("event error = %q, want %q", info.Error, "exit status 1")
			}
			for _, want := range []string{"VERDICT-LINE", "DIAGNOSIS-LINE"} {
				if !strings.Contains(info.Output, want) {
					t.Errorf("event output %q lacks %q — a failure with no line to read", info.Output, want)
				}
			}
			if info.Output != logged {
				t.Errorf("event output %q != logged output %q — the failure travels in two shapes", info.Output, logged)
			}
		})
	}
}

func TestSucceedingToolNodeCarriesItsOutputOnTheEvent(t *testing.T) {
	// Not only the failure path: the field exists so the run view can render
	// in+out, and a success that reports nothing is the same blind spot one
	// exit code later.
	info, logged, err := captureToolCall(t, &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "gate"},
		Language: "sh",
		Script:   "echo GREEN-LINE",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if info.Error != nil {
		t.Fatalf("event carries an error for a node that exited 0: %v", info.Error)
	}
	if !strings.Contains(info.Output, "GREEN-LINE") {
		t.Errorf("event output = %q, want it to carry GREEN-LINE", info.Output)
	}
	if info.Output != logged {
		t.Errorf("event output %q != logged output %q", info.Output, logged)
	}
}
