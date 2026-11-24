package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// Mutations: drop errors.As from IsAmbiguousEffect, or drop its opaque As
// structure walk. The real executor redacts the text without exposing Unwrap;
// recovery must still refuse every undecided cause, including a later sibling.
func TestRedactionPreservesUndecidedEffects(t *testing.T) {
	t.Parallel()
	const token = "synthetic-secret-2660"
	unknown := &sandbox.ExecStatusUnknownError{Detail: "sweep fails " + token}
	settled := &sandbox.ExecStatusUnknownError{Swept: true, Detail: token}
	ordinary := &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: "ordinary " + token}
	persisted := &runtime.RuntimeError{Code: runtime.ErrCodeAmbiguousEffect, Message: "recorded child " + token}
	for _, tc := range []struct {
		name      string
		cause     error
		ambiguous bool
	}{
		{"sandbox", unknown, true},
		{"persisted", persisted, true},
		{"wrapper", &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: token, Cause: unknown}, true},
		{"stored-wrapper", &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: token, Cause: persisted}, true},
		{"joined-sandbox", errors.Join(ordinary, unknown), true},
		{"joined-persisted", errors.Join(ordinary, persisted), true},
		{"settled-first", errors.Join(settled, unknown), true},
		{"wrapped-join", &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: token, Cause: errors.Join(ordinary, persisted)}, true},
		{"joined-wrapper", errors.Join(ordinary, fmt.Errorf("child: %w", persisted)), true},
		{"settled", settled, false},
		{"ordinary", ordinary, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := tool.NewRegistry()
			err := tools.RegisterBuiltin("ambiguous_probe", "probe", nil, func(context.Context, json.RawMessage) (string, error) { return "", tc.cause })
			if err != nil {
				t.Fatal(err)
			}
			guard := secretguard.New([]secretguard.Secret{{Name: "PROBE_TOKEN", Value: token}}, secretguard.DefaultConfig())
			exec := model.NewClawExecutor(model.NewRegistry(), &ir.Workflow{}, model.WithToolRegistry(tools), model.WithSecretGuard(guard))
			_, err = exec.Execute(context.Background(), &ir.ToolNode{BaseNode: ir.BaseNode{ID: "probe"}, Command: "ambiguous_probe"}, nil)
			if err == nil {
				t.Fatal("missing executor failure")
			}
			if strings.Contains(err.Error(), token) || errors.Unwrap(err) != nil {
				t.Fatal("executor exposes the unredacted error")
			}
			action, code := recovery.Dispatch(recovery.DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
			if runtime.IsAmbiguousEffect(err) != tc.ambiguous || (code == runtime.ErrCodeAmbiguousEffect) != tc.ambiguous {
				t.Fatalf("redaction changes ambiguity: type=%T code=%s want ambiguous=%v", err, code, tc.ambiguous)
			}
			if tc.ambiguous && action.Kind != runtime.RecoveryFailTerminal {
				t.Fatalf("redacted undecided effect is retryable: %v", action)
			}
		})
	}
}
