package runtime

import (
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Where a network wording decides a failed command's fate. The command's
// stdout is its DATA: a needle there — a verdict echoed, a test's connection
// refusal — says nothing about the run's connectivity. The reading is the
// command's stderr and exec cause, with the needles too broad for arbitrary
// command output set aside.

// toolNode is the shell node every scenario judges.
func toolNode() ir.Node {
	return &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: "make test"}
}

func TestAToolCommandsNetworkReading(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stdout string
		stderr string
		// failure builds the classified error; nil is a shell failure with
		// exit status 1 and the two streams above.
		failure       func(t *testing.T) error
		wantCode      ErrorCode
		wantCounted   bool
		notCountedWhy string
	}{
		{
			name:        "stderr carries a real network fault",
			stderr:      "curl: (56) Recv failure: Connection reset by peer",
			wantCode:    ErrCodeNetworkTransient,
			wantCounted: false,
		},
		{
			name: "the exec cause carries a dial timeout, streams empty",
			failure: func(*testing.T) error {
				return &model.ToolExecutionError{Cause: &model.ToolCommandFailure{
					NodeID:  "gate",
					Outcome: "shell command failed",
					Cause:   errors.New("dial tcp 10.0.0.1:2376: i/o timeout"),
				}}
			},
			wantCode:    ErrCodeNetworkTransient,
			wantCounted: false,
		},
		{
			// The exec session ended without the command's completion line:
			// the transport ended, not the command, so its EOF is a stream
			// cut and the full list reads it.
			name: "a sandbox exec stream cut, the command's end never proven",
			failure: func(t *testing.T) error {
				return &model.ToolExecutionError{Cause: &model.ToolCommandFailure{
					NodeID:  "gate",
					Outcome: "shell command failed",
					Cause:   errors.Join(exitError(t, 1), &sandbox.ExecStatusUnknownError{Swept: true, Detail: "the pod's command was stopped"}),
					Stderr:  "websocket: close 1006 (abnormal closure): unexpected EOF",
				}}
			},
			wantCode:    ErrCodeNetworkTransient,
			wantCounted: false,
		},
		{
			// The text outside the tool failure is the engine's own, not the
			// command's data: the full list reads it.
			name: "a command failure joined with the engine's own network fault",
			failure: func(t *testing.T) error {
				return errors.Join(shellFailureOn(t, "gate", 1, "", "config: key missing"),
					fmt.Errorf("settle initial usage: %w", errors.New("dial tcp 10.0.0.1:27017: connect: connection refused")))
			},
			wantCode:    ErrCodeNetworkTransient,
			wantCounted: false,
		},
		{
			name: "a failure with no command joined with the engine's own network fault",
			failure: func(*testing.T) error {
				return errors.Join(&model.ToolExecutionError{Cause: errors.New(`model: tool node "gate": registry tool failed`)},
					fmt.Errorf("settle initial usage: %w", errors.New("dial tcp 10.0.0.1:27017: connect: connection refused")))
			},
			wantCode:    ErrCodeNetworkTransient,
			wantCounted: false,
		},
		{
			name: "a failure with no command wrapped by an engine timeout",
			failure: func(*testing.T) error {
				return fmt.Errorf("store: read tcp 10.0.0.1:4222: i/o timeout: %w",
					&model.ToolExecutionError{Cause: errors.New(`model: tool node "gate": registry tool failed`)})
			},
			wantCode:    ErrCodeNetworkTransient,
			wantCounted: false,
		},
		{
			name:          "stdout alone carries a connection refusal",
			stdout:        "    client_test.go:41: dial tcp 127.0.0.1:5432: connect: connection refused",
			wantCode:      ErrCodeExecutionFailed,
			wantCounted:   false,
			notCountedWhy: "the needle sat on stdout; the failure keeps the redeliveries it always had",
		},
		{
			name:          "stderr carries a parser's EOF",
			stderr:        "SyntaxError: unexpected EOF while parsing",
			wantCode:      ErrCodeExecutionFailed,
			wantCounted:   false,
			notCountedWhy: "the wording is a set-aside needle; the streak never counts it",
		},
		{
			name:          "stdout carries the context deadline",
			stdout:        "probe: context deadline exceeded",
			wantCode:      ErrCodeExecutionFailed,
			wantCounted:   false,
			notCountedWhy: "the wording is a set-aside needle on data; the streak never counts it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var failure error = shellFailureOn(t, "gate", 1, tc.stdout, tc.stderr)
			if tc.failure != nil {
				failure = tc.failure(t)
			}
			if got := ClassifyError(failure); got != tc.wantCode {
				t.Errorf("code = %s, want %s", got, tc.wantCode)
			}
			_, _, counted := countedToolFailure(toolNode(), ClassifyError(failure), failure)
			if counted != tc.wantCounted {
				t.Errorf("counted = %v, want %v (%s)", counted, tc.wantCounted, tc.notCountedWhy)
			}
		})
	}
}

// The outage table reads the command's output, not the failure's wrapper:
// a node id that happens to name an outage word must not park the node.
func TestTheOutageTableDoesNotReadTheNodeID(t *testing.T) {
	t.Parallel()
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "check_etimedout"}, Command: "make test"}
	failure := shellFailureOn(t, "check_etimedout", 3, "", "config: key missing")
	if got := ClassifyError(failure); got != ErrCodeExecutionFailed {
		t.Errorf("code = %s, want %s", got, ErrCodeExecutionFailed)
	}
	_, _, counted := countedToolFailure(node, ErrCodeExecutionFailed, failure)
	if !counted {
		t.Error("the failure was not counted — a node id is not the command's output")
	}
}

// Every needle the command reading sets aside is one of the shared list's:
// the subset is a subset, and a host appending to the shared list keeps the
// property without knowing about the command reading.
func TestEverySetAsideNeedleIsInTheSharedList(t *testing.T) {
	t.Parallel()
	shared := map[string]bool{}
	for _, needle := range networkTransientNeedles {
		shared[needle] = true
	}
	for needle := range commandNetworkNeedlesSetAside {
		if !shared[needle] {
			t.Errorf("set-aside needle %q is not in the shared list — the command subset would read a needle the fallback never does", needle)
		}
	}
}

// A tool failure with no command in its chain — a registry tool, an
// `action:`, a refusal before the command started — has no stream to split:
// the whole text keeps the shared reading it always had.
func TestAToolFailureWithNoCommandKeepsTheWholeTextReading(t *testing.T) {
	t.Parallel()
	failure := &model.ToolExecutionError{Cause: errors.New(`model: tool node "n": execute: Post "https://x": unexpected EOF`)}
	if got := ClassifyError(failure); got != ErrCodeNetworkTransient {
		t.Errorf("code = %s, want %s", got, ErrCodeNetworkTransient)
	}
}
