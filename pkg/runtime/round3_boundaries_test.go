package runtime

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/stdiotest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
)

func TestR3ConvergencePreservesChildBudgetHint(t *testing.T) {
	wf := validationWorkflow()
	e := New(wf, tmpStore(t), newStubExecutor())
	child := &RuntimeError{Code: ErrCodeBudgetExceeded, Cause: ErrBudgetExceeded, Hint: "raise the child budget in child.bot before resuming"}
	_, err := e.processConvergence(e.newRunState("fan", nil), "my_agent", []*branchResult{{branchID: "a", err: child}}, nil, nil)
	if !errors.Is(err, ErrBudgetExceeded) || RemedyOf(err).Hint != child.Hint {
		t.Fatalf("child remedy lost: %v / %+v", err, RemedyOf(err))
	}
}

func TestR3StrictToolSchemaIsDeterministic(t *testing.T) {
	wf := validationWorkflow()
	wf.Nodes["my_agent"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "my_agent"}, SchemaFields: ir.SchemaFields{OutputSchema: "MySchema"}}
	e := New(wf, tmpStore(t), newStubExecutor(), WithOutputValidation(true))
	err := e.validateNodeOutput(e.newRunState("strict", nil), "my_agent", wf.Nodes["my_agent"], map[string]any{"summary": true})
	if err == nil || !retrypolicy.IsDeterministic(ClassifyError(err)) {
		t.Fatalf("strict tool can replay: %v disposition=%v", err, retrypolicy.Classify(ClassifyError(err)))
	}
}

func TestR3StrictOutputEnvConventions(t *testing.T) {
	for _, value := range []string{"yes", "on", "y", "enabled", " TRUE ", "1"} {
		t.Setenv(EnvOutputValidation, value)
		if !defaultOutputValidation() {
			t.Errorf("strict %q silently disabled", value)
		}
	}
	t.Setenv(EnvOutputValidation, "typo")
	capture := stdiotest.Run(t, &os.Stderr, func() {
		if defaultOutputValidation() {
			t.Error("unknown value enables strict")
		}
	})
	if !strings.Contains(capture, "OUTPUT_VALIDATION_INVALID") {
		t.Fatalf("missing warning: %+v", capture)
	}
}
