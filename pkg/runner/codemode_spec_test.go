package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The pod's first carrier of the launch-time codemode override (CM-5e,
// #3345): the runner-loop literal that hands msg.Codemode / msg.CodemodeBounds
// to the ExecutorSpec it assembles. It is the ONLY hand-off between the wire
// and BuildExecutor, it is a struct literal a mutant can empty without
// breaking a compile, and the failure direction is the one the v33 bump
// exists to reject — an explicit `off` dropped here would run the capability
// the operator asked away, in silence, with no other red to catch it.
func TestRunMessageCodemodeReachesTheExecutorSpec(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	bounds := &scripthost.Limits{ToolCalls: 4096, Wall: 90 * time.Second, Memory: 1 << 30}
	msg := &queue.RunMessage{
		RunID:          "run-codemode",
		WorkflowName:   "wf",
		Codemode:       "off",
		CodemodeBounds: bounds,
	}
	spec, _, err := r.executorSpec(context.Background(), msg, &ir.Workflow{}, "", iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatalf("executorSpec: %v", err)
	}
	if spec.Codemode != "off" {
		t.Errorf("ExecutorSpec.Codemode = %q, want the message's %q — the pod would run the authored value", spec.Codemode, "off")
	}
	if spec.CodemodeBounds == nil || spec.CodemodeBounds.ToolCalls != 4096 || spec.CodemodeBounds.Wall != 90*time.Second {
		t.Errorf("ExecutorSpec.CodemodeBounds = %+v, want the message's bounds", spec.CodemodeBounds)
	}

	// The absent override stays absent — an old message must not grow one.
	empty, _, err := r.executorSpec(context.Background(), &queue.RunMessage{RunID: "run-plain", WorkflowName: "wf"}, &ir.Workflow{}, "", iterlog.Nop(), nil, nil)
	if err != nil {
		t.Fatalf("executorSpec empty: %v", err)
	}
	if empty.Codemode != "" || empty.CodemodeBounds != nil {
		t.Errorf("a message with no override produced Codemode=%q bounds=%+v — inherit must read absent", empty.Codemode, empty.CodemodeBounds)
	}
}

// The pod's engine feed of the launch-time overrides (tour 2): a message
// carrying no override feeds no option — an absence must stay an absence,
// never a stamped zero.
func TestWithEngineOverrideFeedsOnlyWhatTheMessageCarries(t *testing.T) {
	t.Parallel()
	if got := withEngineOverride("", "", nil); len(got) != 0 {
		t.Errorf("a message with no overrides fed %d options, want 0", len(got))
	}
	if got := withEngineOverride("deny", "", nil); len(got) != 1 {
		t.Errorf("a permission-only message fed %d options, want 1", len(got))
	}
	if got := withEngineOverride("", "off", &scripthost.Limits{ToolCalls: 4096}); len(got) != 1 {
		t.Errorf("a codemode-only message fed %d options, want 1", len(got))
	}
}
