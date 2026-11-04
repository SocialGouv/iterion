package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestNodeFailureKeepsTypedCodeAcrossBoundaries(t *testing.T) {
	for _, surface := range []string{"trunk", "interaction", "subbot"} {
		t.Run(surface, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			wf := validationWorkflow()
			cause := &model.OutputSchemaError{Cause: errors.New("invalid verdict")}
			failure := &RuntimeError{Code: ErrCodeSchemaValidation, Cause: cause, Message: "invalid verdict"}
			exec := newStubExecutor()
			calls := 0
			exec.on("my_agent", func(map[string]any) (map[string]any, error) {
				calls++
				if surface == "interaction" && calls == 1 {
					return nil, &model.ErrNeedsInteraction{NodeID: "my_agent", Backend: "claude_code", SessionID: "session", Questions: map[string]any{"q": "choose"}}
				}
				return nil, fmt.Errorf("backend: %w", failure)
			})
			if surface == "interaction" {
				wf.Nodes["my_agent"].(*ir.AgentNode).Interaction = ir.InteractionHuman
			}
			if surface == "subbot" {
				wf.Nodes["my_agent"] = &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "my_agent"}, Source: "child.bot"}
			}
			st := tmpStore(t)
			eng := New(wf, st, exec, WithSubbotRunner(func(context.Context, SubbotRequest) (map[string]any, error) { return nil, failure }))
			ctx := context.Background()
			err := eng.Run(ctx, "typed-boundary", nil)
			if surface == "interaction" {
				if !errors.Is(err, ErrRunPaused) {
					t.Fatalf("pause: %v", err)
				}
				err = eng.Resume(ctx, "typed-boundary", map[string]any{"q": "answer"})
			}
			run, loadErr := st.LoadRun(ctx, "typed-boundary")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			var rt *RuntimeError
			if !errors.As(err, &rt) || rt.Code != ErrCodeSchemaValidation || run.FailureCode != ErrCodeSchemaValidation || !errors.Is(err, cause) {
				t.Fatalf("surface=%s stored=%s err=%v cause preserved=%v", surface, run.FailureCode, err, errors.Is(err, cause))
			}
		})
	}
}
