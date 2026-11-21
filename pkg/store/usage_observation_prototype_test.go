package store_test

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"testing"
)

func TestPrototypeObservationFS(t *testing.T) {
	storetest.RunUsageObservationPrototype(t, func(t *testing.T) store.RunStore {
		s, e := store.New(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
func TestPrototypeObservationFSPricing(t *testing.T) {
	storetest.RunUsageObservationPricingPrototype(t, func(t *testing.T) store.RunStore {
		s, e := store.New(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}

func TestPrototypeObservationIteration(t *testing.T) {
	storetest.RunUsageIterationPrototype(t, func(t *testing.T) store.RunStore {
		s, e := store.New(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return s
	})
}
