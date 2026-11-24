package mongo

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// boundInheritingMongo embeds the native Mongo store: it inherits the
// endorsement method while overriding a read it would vouch for.
type boundInheritingMongo struct{ *Store }

func (s boundInheritingMongo) LoadRun(_ context.Context, id string) (*store.Run, error) {
	return s.Store.LoadRun(context.Background(), id)
}

// W18: the native Mongo store endorses itself and nothing else: an embedding
// wrapper, another Mongo store, the filesystem store and a nil store are
// refused typed. CI's mongo-conformance job supplies the URI.
func TestBoundObservationStoreNativeMongo(t *testing.T) {
	t.Parallel()
	s, _ := valueFormatMongo(t, false)
	if err := store.RequireBoundObservationStore(s); err != nil {
		t.Fatalf("native Mongo store refused: %v", err)
	}
	other, _ := valueFormatMongo(t, false)
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var typedNil *Store
	for name, st := range map[string]store.RunStore{
		"typed-nil":                 typedNil,
		"embedding-wrapper":         boundInheritingMongo{s},
		"pointer-embedding-wrapper": &boundInheritingMongo{s},
	} {
		if err := store.RequireBoundObservationStore(st); !errors.Is(err, store.ErrRunDataCapability) {
			t.Errorf("%s admitted: %v", name, err)
		}
	}
	for name, actual := range map[string]store.RunStore{"another-mongo": other, "filesystem": fs, "wrapper": boundInheritingMongo{s}} {
		if err := s.CheckBoundObservationStore(actual); !errors.Is(err, store.ErrRunDataCapability) {
			t.Errorf("Mongo store endorsed %s: %v", name, err)
		}
	}
}
