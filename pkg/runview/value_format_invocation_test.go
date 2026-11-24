package runview

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildExecutorRequiresBornOriginalFormatBeforeEffects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		actual int
		bound  int
		id     string
		want   error
	}{
		{"unbound born legacy", 1, 0, "run", store.ErrValueFormatCapability},
		{"bound but absent", 0, 1, "run", store.ErrRunNotFound},
		{"legacy original exact successor", 2, 1, "run", store.ErrValueFormatChanged},
		{"exact original legacy successor", 1, 2, "run", store.ErrValueFormatChanged},
		{"wrong ID", 1, 1, "other", store.ErrValueFormatChanged},
		{"legacy positive", 1, 1, "run", nil}, {"exact positive", 2, 2, "run", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if tc.actual != 0 {
				if _, err = store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "run", Format: store.ValueFormat(tc.actual)}); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			if tc.bound != 0 {
				ctx, err = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: tc.id, Format: store.ValueFormat(tc.bound)})
				if err != nil {
					t.Fatal(err)
				}
			}
			hooks := 0
			exec, err := BuildExecutor(ExecutorSpec{Ctx: ctx, Store: s, RunID: "run", Workflow: &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{}}, SecretGuardSink: func(*secretguard.Guard) { hooks++ }})
			if exec != nil {
				defer exec.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want %v", err, tc.want)
			}
			if tc.want != nil {
				if exec != nil || hooks != 0 {
					t.Fatalf("refusal effects: executor=%v hooks=%d", exec, hooks)
				}
				if _, e := os.Stat(filepath.Join(s.Root(), "runs", "run", "artifact_files")); !os.IsNotExist(e) {
					t.Fatalf("refusal made files: %v", e)
				}
			} else if exec == nil || hooks != 1 {
				t.Fatalf("positive: exec=%v hooks=%d", exec, hooks)
			}
		})
	}
}
func TestPreparedInvocationCannotAdoptRecreatedFormat(t *testing.T) {
	t.Parallel()
	for _, from := range []store.ValueFormat{1, 2} {
		t.Run(string(rune('0'+from)), func(t *testing.T) {
			t.Parallel()
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "run", Format: from}); err != nil {
				t.Fatal(err)
			}
			inv, err := store.PrepareRunInvocation(t.Context(), s, store.ExistingRunInvocation{ID: "run"})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.DeleteRun(inv.Context, "run"); err != nil {
				t.Fatal(err)
			}
			if n, err := s.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil || n != 1 {
				t.Fatalf("prune=%d %v", n, err)
			}
			to := store.ValueFormat(3) - from
			if _, err = store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "run", Format: to}); err != nil {
				t.Fatal(err)
			}
			hooks := 0
			ex, err := BuildExecutor(ExecutorSpec{Ctx: inv.Context, Store: s, RunID: "run", Workflow: &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{}}, SecretGuardSink: func(*secretguard.Guard) { hooks++ }})
			if ex != nil {
				defer ex.Close()
			}
			if !errors.Is(err, store.ErrValueFormatChanged) || ex != nil || hooks != 0 {
				t.Fatalf("successor escaped: %v %v %d", ex, err, hooks)
			}
		})
	}
}
func TestPrepareSubbotInvocationPreservesParentAndRefusesDowngrade(t *testing.T) {
	t.Parallel()
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(string(rune('0'+format)), func(t *testing.T) {
			t.Parallel()
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			r, err := store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "parent", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			inv, err := PrepareSubbotInvocation(ctx, s, "parent", store.RunCreateSpec{ID: "child", Inputs: map[string]any{"a": "b"}})
			if format == 2 {
				if !errors.Is(err, store.ErrValueFormatCapability) {
					t.Fatal(err)
				}
				if _, e := s.LoadRun(t.Context(), "child"); !errors.Is(e, store.ErrRunNotFound) {
					t.Fatal(e)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if inv.Run.ParentRunID != "parent" {
					t.Fatal(inv.Run)
				}
				if err = store.RequireRunValueFormat(inv.Context, "child"); err != nil {
					t.Fatal(err)
				}
			}
			if err = store.RequireRunValueFormat(ctx, "parent"); err != nil {
				t.Fatal("parent context replaced", err)
			}
		})
	}
}
