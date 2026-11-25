package model

import (
	"context"
	"errors"
	"os/exec"
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
	t.Parallel()
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
	t.Parallel()
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

// The streams travel apart inside the failure (ToolCommandFailure), so a
// reader that judges the command reads its stderr and exec cause without
// ever taking stdout — the command's DATA — for a fact about the run. The
// quoted text keeps its exact shape: the run document, the fingerprint and
// the event log all read it.
func TestAFailedCommandKeepsItsStreamsApart(t *testing.T) {
	t.Parallel()
	// Both streams: a gate prints its verdict on stdout and its diagnosis
	// on stderr.
	const body = "echo VERDICT-LINE; echo DIAGNOSIS-LINE 1>&2; exit 1"

	cases := []struct {
		name     string
		node     *ir.ToolNode
		outcome  string
		errorLit string
	}{
		{"script", &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "gate"},
			Language: "sh",
			Script:   body,
		}, "script failed",
			"model: tool node \"gate\": script failed: exit status 1\nstdout: VERDICT-LINE\n\nstderr: DIAGNOSIS-LINE\n"},
		{"shell", &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "gate"},
			Command:  body,
		}, "shell command failed",
			"model: tool node \"gate\": shell command failed: exit status 1\nstdout: VERDICT-LINE\n\nstderr: DIAGNOSIS-LINE\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := captureToolCall(t, tc.node)
			if err == nil {
				t.Fatal("Execute returned nil error for a node that exited 1")
			}
			var cmd *ToolCommandFailure
			if !errors.As(err, &cmd) {
				t.Fatalf("the chain carries no *ToolCommandFailure: %v", err)
			}
			if cmd.NodeID != "gate" {
				t.Errorf("NodeID = %q, want %q", cmd.NodeID, "gate")
			}
			if cmd.Outcome != tc.outcome {
				t.Errorf("Outcome = %q, want %q", cmd.Outcome, tc.outcome)
			}
			if cmd.Stdout != "VERDICT-LINE\n" {
				t.Errorf("Stdout = %q, want %q", cmd.Stdout, "VERDICT-LINE\n")
			}
			if cmd.Stderr != "DIAGNOSIS-LINE\n" {
				t.Errorf("Stderr = %q, want %q", cmd.Stderr, "DIAGNOSIS-LINE\n")
			}
			// Diagnostics is the command's report of WHY, never its stdout
			// and never the wrapper.
			diag := cmd.Diagnostics()
			if strings.Contains(diag, "VERDICT-LINE") {
				t.Errorf("Diagnostics = %q — it carries the command's stdout", diag)
			}
			if strings.Contains(diag, "gate") {
				t.Errorf("Diagnostics = %q — it carries the wrapper's node id", diag)
			}
			if !strings.Contains(diag, "DIAGNOSIS-LINE") || !strings.Contains(diag, "exit status 1") {
				t.Errorf("Diagnostics = %q, want the exec cause and stderr", diag)
			}
			// Output adds stdout, still without the wrapper.
			out := cmd.Output()
			if !strings.Contains(out, "VERDICT-LINE") || !strings.Contains(out, "DIAGNOSIS-LINE") {
				t.Errorf("Output = %q, want both streams", out)
			}
			if strings.Contains(out, "model: tool node") {
				t.Errorf("Output = %q — it carries the wrapper's own text", out)
			}
			// The quoted text is byte-for-byte what the executor has always
			// printed; the run document and the fingerprint read it.
			if got := err.Error(); got != tc.errorLit {
				t.Errorf("Error() = %q, want %q", got, tc.errorLit)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Errorf("the exit status is no longer found through the failure: %v", err)
			}
		})
	}
}

// The three failure shapes quote the text their fmt.Errorf built before the
// streams travelled apart: a reader diffing two executions of an unchanged
// failure — the fingerprint, the run document, the event log — must see no
// change.
func TestAFailureQuotesTheTextItAlwaysQuoted(t *testing.T) {
	t.Parallel()
	runErr := func(t *testing.T) error {
		t.Helper()
		runErr := exec.Command("sh", "-c", "exit 7").Run()
		if runErr == nil {
			t.Fatal("scenario broken: `exit 7` succeeded")
		}
		return runErr
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"shell", (&ToolCommandFailure{NodeID: "gate", Outcome: "shell command failed", Cause: runErr(t), Stdout: "OUT", Stderr: "ERR"}).Error(),
			"model: tool node \"gate\": shell command failed: exit status 7\nstdout: OUT\nstderr: ERR"},
		{"script", (&ToolCommandFailure{NodeID: "gate", Outcome: "script failed", Cause: runErr(t), Stdout: "", Stderr: "ERR"}).Error(),
			"model: tool node \"gate\": script failed: exit status 7\nstdout: \nstderr: ERR"},
		{"verified-action-recipe-failed", (&ToolCommandFailure{NodeID: "gate", Outcome: "postcondition not met (policy: recover) after self_repair; the recipe failed", Cause: runErr(t), Stdout: "OUT", Stderr: "ERR"}).Error(),
			"model: tool node \"gate\": postcondition not met (policy: recover) after self_repair; the recipe failed: exit status 7\nstdout: OUT\nstderr: ERR"},
		{"verified-action-postcondition-only", (&ToolCommandFailure{NodeID: "gate", Outcome: "postcondition not met (policy: required) after recipe", Stdout: "OUT", Stderr: "ERR"}).Error(),
			"model: tool node \"gate\": postcondition not met (policy: required) after recipe\nstdout: OUT\nstderr: ERR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("Error() = %q, want %q", tc.got, tc.want)
			}
		})
	}
}
