package orgsweep

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeRunStore stands in for the cloud run store on the three methods the
// purge calls, under the same terms: a call without a tenant in the context
// is refused (the real listing refuses, the real store panics elsewhere), a
// listing sees only the tenant's live runs, a load answers ErrRunNotFound for
// a run that is gone or another tenant's, and a delete takes the run's blobs
// with it — the contract pkg/store/mongo's DeleteRun proves against its own
// blob fake.
type fakeRunStore struct {
	mu       sync.Mutex
	tenants  map[string]string          // run id → tenant
	statuses map[string]store.RunStatus // run id → lifecycle status
	blobs    map[string]int             // run id → objects in the bucket
	failing  map[string]error           // run id → the error its delete returns
	listed   []string                   // the tenants a listing ran under, in order
	deleted  []string                   // run ids deleted, in order
	attempts []string                   // run ids a DeleteRun was called on, in order
	// onDelete, when set, runs after each successful delete, outside the
	// lock: the hook a test uses to change the world mid-purge.
	onDelete func(id string)
}

func newFakeRunStore() *fakeRunStore {
	return &fakeRunStore{
		tenants:  map[string]string{},
		statuses: map[string]store.RunStatus{},
		blobs:    map[string]int{},
		failing:  map[string]error{},
	}
}

// addRun seeds one finished run of tenant with n blobs.
func (f *fakeRunStore) addRun(id, tenant string, n int) {
	f.addRunWithStatus(id, tenant, n, store.RunStatusFinished)
}

// addRunWithStatus seeds one run of tenant with n blobs in the given status.
func (f *fakeRunStore) addRunWithStatus(id, tenant string, n int, status store.RunStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants[id] = tenant
	f.statuses[id] = status
	f.blobs[id] = n
}

func (f *fakeRunStore) ListRunIDsForPurge(ctx context.Context) ([]string, error) {
	tenant, ok := store.TenantFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("fakeRunStore: listing without a tenant in ctx")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, tenant)
	var ids []string
	for id, t := range f.tenants {
		if t == tenant {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (f *fakeRunStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	tenant, ok := store.TenantFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("fakeRunStore: LoadRun without a tenant in ctx")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner, exists := f.tenants[id]; !exists || owner != tenant {
		return nil, fmt.Errorf("fakeRunStore: run %s: %w", id, store.ErrRunNotFound)
	}
	return &store.Run{ID: id, Status: f.statuses[id]}, nil
}

func (f *fakeRunStore) DeleteRun(ctx context.Context, id string) error {
	tenant, ok := store.TenantFromContext(ctx)
	if !ok {
		return fmt.Errorf("fakeRunStore: DeleteRun without a tenant in ctx")
	}
	f.mu.Lock()
	f.attempts = append(f.attempts, id)
	if err := f.failing[id]; err != nil {
		f.mu.Unlock()
		return err
	}
	if owner, exists := f.tenants[id]; exists && owner != tenant {
		f.mu.Unlock()
		return fmt.Errorf("fakeRunStore: run %s belongs to tenant %s, deleted under %s", id, owner, tenant)
	}
	delete(f.tenants, id)
	delete(f.statuses, id)
	delete(f.blobs, id)
	f.deleted = append(f.deleted, id)
	hook := f.onDelete
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return nil
}

// objects is the number of blobs still in the bucket, all runs together.
func (f *fakeRunStore) objects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.blobs {
		n += c
	}
	return n
}

// runsOf is the number of runs the store still holds for tenant.
func (f *fakeRunStore) runsOf(tenant string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.tenants {
		if t == tenant {
			n++
		}
	}
	return n
}
