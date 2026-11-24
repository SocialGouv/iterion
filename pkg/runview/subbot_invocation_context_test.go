package runview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPrepareSubbotInvocationRequiresOriginalParentBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		format  store.ValueFormat
		boundID string
		cancel  bool
		want    error
	}{
		{"legacy bound", 1, "parent", false, nil},
		{"exact bound", 2, "parent", false, store.ErrValueFormatCapability},
		{"foreign binding", 1, "foreign", false, store.ErrValueFormatChanged},
		{"canceled legacy", 1, "parent", true, context.Canceled},
		{"unbound exact parent", 2, "", false, store.ErrValueFormatCapability},
		{"unbound legacy parent", 1, "", false, store.ErrValueFormatCapability},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			if _, e = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "parent", Format: tc.format}); e != nil {
				t.Fatal(e)
			}
			type traceKey struct{}
			ctx, cancel := context.WithDeadline(context.WithValue(t.Context(), traceKey{}, "trace"), time.Now().Add(time.Minute))
			defer cancel()
			ctx = model.WithExecutionUsageCollector(ctx, &model.ExecutionUsageCollector{})
			ctx = model.WithOutputCorrectionOwnership(ctx, model.OutputCorrectionOwnership{Protocol: model.OutputCorrectionProtocol, Authority: store.RunExecutionAuthority{RunID: "parent", Epoch: "parent-epoch"}})
			if tc.boundID != "" {
				ctx, e = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: tc.boundID, Format: tc.format})
				if e != nil {
					t.Fatal(e)
				}
			}
			if tc.cancel {
				cancel()
			}
			inv, e := PrepareSubbotInvocation(ctx, st, "parent", store.RunCreateSpec{ID: "child", Inputs: map[string]any{"x": "child"}})
			t.Logf("error=%v ownedBirth=%v", e, inv.OwnedBirth)
			if tc.want != nil {
				if !errors.Is(e, tc.want) {
					t.Fatalf("CHILD_PARENT_BINDING_REFUSAL error=%v want=%v", e, tc.want)
				}
				if _, e = st.LoadRun(t.Context(), "child"); !errors.Is(e, store.ErrRunNotFound) {
					t.Fatalf("child created despite refusal: %v", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = store.RequireRunValueFormat(inv.Context, "child"); e != nil {
				t.Fatal(e)
			}
			if e = store.RequireRunValueFormat(ctx, "parent"); e != nil {
				t.Fatal(e)
			}
			if inv.Run.ParentRunID != "parent" || inv.Run.FormatVersion != 1 || inv.Run.Inputs["x"] != "child" {
				t.Fatal(inv.Run)
			}
			if own, ok := model.OutputCorrectionOwnershipFromContext(inv.Context); ok {
				t.Fatalf("PARENT_AUTHORITY_INHERITED %+v", own)
			}
			if _, ok := model.OutputCorrectionOwnershipFromContext(ctx); !ok {
				t.Fatal("parent ownership erased")
			}
			if inv.Context.Value(traceKey{}) != "trace" {
				t.Fatal("trace lost")
			}
			want, _ := ctx.Deadline()
			got, _ := inv.Context.Deadline()
			if got != want {
				t.Fatal("deadline lost")
			}
		})
	}
}
