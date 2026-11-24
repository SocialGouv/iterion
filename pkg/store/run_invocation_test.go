package store

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"testing"
)

func TestPrepareRunInvocationExclusiveModes(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err = PrepareRunInvocation(ctx, s, ExistingRunInvocation{ID: "missing"}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("existing missing: %v", err)
	}
	prepared, err := PrepareRunInvocation(ctx, s, NewLegacyRunInvocation{Spec: RunCreateSpec{ID: "new", WorkflowName: "wf", ParentRunID: "parent", Inputs: map[string]any{"value": "first"}, Status: RunStatusQueued}})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.OwnedBirth || prepared.Run.ParentRunID != "parent" || prepared.Run.Status != RunStatusQueued || prepared.Run.FormatVersion != 1 {
		t.Fatalf("birth=%+v", prepared)
	}
	if err = RequireRunValueFormat(prepared.Context, "new"); err != nil {
		t.Fatal(err)
	}
	existing, err := PrepareRunInvocation(prepared.Context, s, ExistingRunInvocation{ID: "new"})
	if err != nil || existing.OwnedBirth {
		t.Fatalf("existing=%+v %v", existing, err)
	}
	if !reflect.DeepEqual(existing.Run.Inputs, map[string]any{"value": "first"}) {
		t.Fatal(existing.Run.Inputs)
	}
	duplicate, err := PrepareRunInvocation(ctx, s, NewLegacyRunInvocation{Spec: RunCreateSpec{ID: "new", Inputs: map[string]any{"value": "overwritten"}}})
	if !errors.Is(err, fs.ErrExist) || duplicate.OwnedBirth || duplicate.Context != nil {
		t.Fatalf("duplicate adopted: %+v %v", duplicate, err)
	}
	fresh, err := s.LoadRun(ctx, "new")
	if err != nil || fresh.Inputs["value"] != "first" {
		t.Fatalf("duplicate changed row: %+v %v", fresh, err)
	}
	if _, err = PrepareRunInvocation(ctx, s, NewLegacyRunInvocation{Spec: RunCreateSpec{ID: "exact", Format: ExactValueFormat}}); !errors.Is(err, ErrValueFormatCapability) {
		t.Fatalf("exact birth: %v", err)
	}
	if _, err = s.LoadRun(ctx, "exact"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("exact birth wrote: %v", err)
	}
	if err = RequireRunValueFormat(ctx, "new"); !errors.Is(err, ErrValueFormatCapability) {
		t.Fatal("unbound became captured legacy", err)
	}
}

type invocationCreateErrorStore struct {
	RunStore
	err     error
	creates int
}

func (s *invocationCreateErrorStore) CreateRun(context.Context, string, string, map[string]any) (*Run, error) {
	s.creates++
	return nil, s.err
}
func TestPrepareRunInvocationPreservesCreateErrors(t *testing.T) {
	t.Parallel()
	infra := errors.New("write unavailable")
	s := &invocationCreateErrorStore{err: infra}
	got, err := PrepareRunInvocation(t.Context(), s, NewLegacyRunInvocation{Spec: RunCreateSpec{ID: "new"}})
	if !errors.Is(err, infra) || got.OwnedBirth || got.Context != nil || s.creates != 1 {
		t.Fatalf("got=%+v err=%v creates=%d", got, err, s.creates)
	}
}

type invocationCancelAfterCreate struct {
	RunStore
	cancel context.CancelFunc
}

func (s *invocationCancelAfterCreate) CreateRun(ctx context.Context, id, name string, inputs map[string]any) (*Run, error) {
	r, err := s.RunStore.CreateRun(ctx, id, name, inputs)
	if err == nil {
		s.cancel()
	}
	return r, err
}
func TestPrepareRunInvocationRetainsOnlySuccessfulBirthOnCancellation(t *testing.T) {
	t.Parallel()
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spy := &invocationCancelAfterCreate{RunStore: st, cancel: cancel}
	inv, err := PrepareRunInvocation(ctx, spy, NewLegacyRunInvocation{Spec: RunCreateSpec{ID: "created"}})
	if !errors.Is(err, context.Canceled) || !inv.OwnedBirth || inv.Context == nil {
		t.Fatalf("invocation=%+v error=%v", inv, err)
	}
	if err := RequireRunValueFormat(context.WithoutCancel(inv.Context), "created"); err != nil {
		t.Fatal(err)
	}
	if format, err := ValueFormatForWrite(context.WithoutCancel(inv.Context), "created"); err != nil || format != LegacyValueFormat {
		t.Fatalf("original=%v %v", format, err)
	}
	if _, err = st.LoadRun(t.Context(), "created"); err != nil {
		t.Fatal(err)
	}
}
