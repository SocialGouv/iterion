package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestHumanSchemaIsStrictlyValidatedWhenRequested(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	wf := validationWorkflow()
	wf.Nodes["my_agent"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "my_agent"}, SchemaFields: ir.SchemaFields{OutputSchema: "MySchema"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	exec := model.NewClawExecutor(model.NewRegistry(), wf)
	eng := New(wf, tmpStore(t), exec, WithOutputCorrectionBudget(0), WithOutputValidation(true))
	_, err := eng.correctAndValidateNodeOutput(context.Background(), &runState{runID: "human-schema"}, "my_agent", wf.Nodes["my_agent"], map[string]any{"summary": "ok", "score": "not-an-integer"})
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeSchemaValidation {
		t.Fatalf("human output bypasses schema: %v", err)
	}
}
