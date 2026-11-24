package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResourceInvocationPreservesChildBirthContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format store.ValueFormat
		bound  string
		cancel bool
		want   error
	}{
		{"legacy", 1, "parent", false, nil},
		{"exact no downgrade", 2, "parent", false, store.ErrValueFormatCapability},
		{"foreign no adoption", 1, "foreign", false, store.ErrValueFormatChanged},
		{"unbound no adoption", 2, "", false, store.ErrValueFormatCapability},
		{"canceled no birth", 1, "parent", true, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tmpStore(t)
			if _, err := st.CreateRun(t.Context(), "parent", "fixture", nil); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.bound != "" {
				var err error
				ctx, err = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: tc.bound, Format: tc.format})
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx = model.WithOutputCorrectionOwnership(ctx, model.OutputCorrectionOwnership{Protocol: model.OutputCorrectionProtocol, Authority: store.RunExecutionAuthority{RunID: "parent", Epoch: "parent-epoch"}})
			if tc.cancel {
				cancel()
			}
			inv, err := prepareResourceInvocation(ctx, st, "child", "parent")
			if !errors.Is(err, tc.want) {
				t.Fatalf("CHILD_BIRTH_REFUSAL err=%v want=%v", err, tc.want)
			}
			if tc.want != nil {
				if _, err = st.LoadRun(t.Context(), "child"); !errors.Is(err, store.ErrRunNotFound) {
					t.Fatal("child born despite refusal", err)
				}
				return
			}
			if inv.Run.ParentRunID != "parent" || inv.Run.FormatVersion != 1 {
				t.Fatal(inv.Run)
			}
			if own, ok := model.OutputCorrectionOwnershipFromContext(inv.Context); ok {
				t.Fatalf("PARENT_AUTHORITY_INHERITED %+v", own)
			}
			if err = store.RequireRunValueFormat(ctx, "parent"); err != nil {
				t.Fatal("parent changed", err)
			}
		})
	}
}
