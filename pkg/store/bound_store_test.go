package store

import (
	"context"
	"errors"
	"testing"
)

// boundCtxDiscardingStore decorates a store and drops the caller's context on
// LoadRun. Embedding the RunStore interface promotes no endorsement.
type boundCtxDiscardingStore struct{ RunStore }

func (s boundCtxDiscardingStore) LoadRun(_ context.Context, id string) (*Run, error) {
	return s.RunStore.LoadRun(context.Background(), id)
}

// boundInheritingStore embeds the native store: it inherits the endorsement
// method while overriding a read it would vouch for.
type boundInheritingStore struct{ *FilesystemRunStore }

func (s boundInheritingStore) LoadRun(_ context.Context, id string) (*Run, error) {
	return s.FilesystemRunStore.LoadRun(context.Background(), id)
}

// boundCarelessStore endorses whatever it is asked about, its own nil
// receiver included: only RequireBoundObservationStore's nil guard refuses a
// typed-nil one.
type boundCarelessStore struct{ RunStore }

func (*boundCarelessStore) CheckBoundObservationStore(RunStore) error { return nil }

// W18: the native filesystem store endorses itself and nothing else; a
// missing store, a decorator without the capability and an embedding wrapper
// are refused typed.
func TestRequireBoundObservationStoreAdmitsOnlyItsReceiver(t *testing.T) {
	t.Parallel()
	native, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireBoundObservationStore(native); err != nil {
		t.Fatalf("native store refused: %v", err)
	}
	var typedNil *FilesystemRunStore
	var carelessNil *boundCarelessStore
	for name, st := range map[string]RunStore{
		"nil":                       nil,
		"typed-nil":                 typedNil,
		"careless-typed-nil":        carelessNil,
		"ctx-discarding":            boundCtxDiscardingStore{native},
		"embedding-wrapper":         boundInheritingStore{native},
		"pointer-embedding-wrapper": &boundInheritingStore{native},
	} {
		if err := RequireBoundObservationStore(st); !errors.Is(err, ErrRunDataCapability) {
			t.Errorf("%s admitted: %v", name, err)
		}
	}
	other, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := native.CheckBoundObservationStore(other); !errors.Is(err, ErrRunDataCapability) {
		t.Errorf("another filesystem store endorsed: %v", err)
	}
	if err := native.CheckBoundObservationStore(boundInheritingStore{native}); !errors.Is(err, ErrRunDataCapability) {
		t.Errorf("a wrapper of this store endorsed: %v", err)
	}
	if err := typedNil.CheckBoundObservationStore(typedNil); !errors.Is(err, ErrRunDataCapability) {
		t.Errorf("a nil receiver endorsed itself: %v", err)
	}
}
