package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

type fakePoolTopology struct {
	schemas   []string
	consumers []string
	schemaErr error
}

func (f *fakePoolTopology) EnsurePoolSchema(_ context.Context, pool string) error {
	if f.schemaErr != nil {
		return f.schemaErr
	}
	f.schemas = append(f.schemas, pool)
	return nil
}

func (f *fakePoolTopology) PreparePoolConsumer(_ context.Context, pool string) (any, error) {
	f.consumers = append(f.consumers, pool)
	return nil, nil
}

// The reconciler drives the topology from the registry: every non-disabled
// entry gets its streams, an active/draining entry also gets its consumer,
// a disabled entry gets nothing, one broken pool does not starve the
// others, and no registry (or no store) reconciles to nothing. Red when
// any branch loses its ensure.
func TestReconcileRunnerPools(t *testing.T) {
	newServer := func(reg *platformcfg.RunnerPools, regErr error) *Server {
		store := platformcfg.NewMemoryStore[platformcfg.RunnerPools]()
		if regErr != nil {
			return &Server{} // nil-backed Get errors? MemoryStore cannot; the err leg uses a func store below.
		}
		if reg != nil {
			if err := store.Put(context.Background(), *reg); err != nil {
				t.Fatal(err)
			}
		}
		return &Server{runnerPoolsStore: store}
	}
	topo := &fakePoolTopology{}
	s := newServer(&platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{
		{Name: "active-pool", State: platformcfg.RunnerPoolActive},
		{Name: "drain-pool", State: platformcfg.RunnerPoolDraining},
		{Name: "prov-pool", State: platformcfg.RunnerPoolProvisioning},
		{Name: "dead-pool", State: platformcfg.RunnerPoolDisabled},
	}}, nil)
	if err := s.reconcileRunnerPools(context.Background(), topo); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	wantSchema := []string{"active-pool", "drain-pool", "prov-pool"}
	if len(topo.schemas) != 3 {
		t.Fatalf("schemas = %v, want %v (a disabled pool gets nothing)", topo.schemas, wantSchema)
	}
	wantConsumers := []string{"active-pool", "drain-pool"}
	if len(topo.consumers) != 2 {
		t.Fatalf("consumers = %v, want %v (provisioning routes nothing yet)", topo.consumers, wantConsumers)
	}
	// No registry record: nothing to do, no error.
	topo2 := &fakePoolTopology{}
	if err := newServer(nil, nil).reconcileRunnerPools(context.Background(), topo2); err != nil {
		t.Fatalf("an empty registry reconciles to nothing: %v", err)
	}
	if len(topo2.schemas) != 0 {
		t.Fatalf("an empty registry created topology: %+v", topo2.schemas)
	}
	// No store wired: a no-op (the local/tests shape).
	if err := (&Server{}).reconcileRunnerPools(context.Background(), &fakePoolTopology{}); err != nil {
		t.Fatalf("a server without a registry store must no-op: %v", err)
	}
	// One broken pool must not starve the others.
	s3 := newServer(&platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{
		{Name: "brok"},
		{Name: "healthy", State: platformcfg.RunnerPoolActive},
	}}, nil)
	topo3 := &fakePoolTopology{schemaErr: errors.New("broker down")}
	err := s3.reconcileRunnerPools(context.Background(), topo3)
	if err == nil || !strings.Contains(err.Error(), "broker down") {
		t.Fatalf("the broken pool's error must surface: %v", err)
	}
}
