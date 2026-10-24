package runview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Mutation: return the child's text as fmt.Errorf, dropping FailureCode, in
// AwaitSubbotTerminal. Recovery must read the stored code through the parent
// executor's catch-all wrapper and refuse its retry recipe.
func TestAwaitSubbotPreservesFailureCode(t *testing.T) {
	for _, code := range []store.FailureCode{store.FailureAmbiguousEffect, store.FailureAuthFailed, ""} {
		t.Run(string(code), func(t *testing.T) {
			s := mustStore(t)
			ctx := context.Background()
			if _, err := s.CreateRun(ctx, "child", "c", nil); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateRunStatusCoded(ctx, "child", store.RunStatusFailedResumable, "child diagnosis", code); err != nil {
				t.Fatal(err)
			}
			_, err := AwaitSubbotTerminal(ctx, s, "child", nil)
			var rt *runtime.RuntimeError
			if !errors.As(err, &rt) || rt.Code != code || !strings.Contains(err.Error(), "child diagnosis") {
				t.Fatalf("child's stored code %q is lost: %v", code, err)
			}
			if code != store.FailureAmbiguousEffect {
				return
			}
			wrapped := &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: "subbot failed", Cause: fmt.Errorf("executor: %w", err)}
			classified := recovery.Classify(wrapped)
			if classified != runtime.ErrCodeAmbiguousEffect {
				t.Fatalf("recovery classifies the child as %s", classified)
			}
			if act := recovery.DefaultRecipes()[classified].Apply(ctx, rt, 0); act.Kind != recovery.ActionFailTerminal {
				t.Fatalf("recovery re-drives the child: %v", act)
			}
		})
	}
}

// Mutation: remove the AMBIGUOUS_EFFECT exception in ReattachSubbotChild's
// failed-child arm. Reattachment must keep the binding and never spawn fresh.
func TestReattachSubbotKeepsUndecidedChild(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			s := mustStore(t)
			ctx := context.Background()
			for _, id := range []string{"parent", "child"} {
				if _, err := s.CreateRun(ctx, id, id, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.SetSubbotChild(ctx, "parent", "invoke", "child"); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateRunStatusCoded(ctx, "child", status, "may still run", store.FailureAmbiguousEffect); err != nil {
				t.Fatal(err)
			}
			_, err, handled := ReattachSubbotChild(ctx, s, runtime.SubbotRequest{ParentRunID: "parent", ReattachKey: "invoke"}, nil)
			if !handled || !runtime.IsAmbiguousEffect(err) {
				t.Errorf("reattach permits a new child: handled=%v, err=%v", handled, err)
			}
			p, err := s.LoadRun(ctx, "parent")
			if err != nil {
				t.Fatal(err)
			}
			if p.SubbotChildren["invoke"] != "child" {
				t.Errorf("undecided child's binding is lost: %v", p.SubbotChildren)
			}
		})
	}
}
