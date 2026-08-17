package runview

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// localUsageStore is the process-local record of what this machine has
// learned about its own subscription windows. A cloud pod never reaches it
// (the runner injects a Mongo-backed guard instead); a laptop or a
// single-process studio has nothing else, and sharing readings between two
// runs of the same process is precisely the useful part — the second run
// starts already knowing what the first one measured.
var (
	localUsageStoreOnce sync.Once
	localUsageStore     *usagecap.MemStore
)

func processUsageStore() *usagecap.MemStore {
	localUsageStoreOnce.Do(func() { localUsageStore = usagecap.NewMemStore() })
	return localUsageStore
}

// resolveUsageGuard returns the guard governing this run: the caller's when
// it injected one (the cloud runner, which knows which credential the run
// draws on), otherwise one built from the machine-wide policy.
//
// A malformed policy is an error rather than an absent cap: every wrong
// answer here fails open, and a guard silently disabled by a typo is the
// failure the whole package exists to prevent.
func resolveUsageGuard(spec ExecutorSpec) (*usagecap.Guard, error) {
	if spec.UsageGuard != nil {
		return spec.UsageGuard, nil
	}
	pol, err := usagecap.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("runview: usage cap: %w", err)
	}
	if !pol.Enabled() {
		return nil, nil
	}
	store := processUsageStore()
	key := usagecap.Key("", usagecap.ScopeLocal)
	if spec.Logger != nil {
		spec.Logger.Info("usage cap armed: %s", pol)
	}
	return usagecap.NewGuard(pol, func(r usagecap.Reading) {
		// Best effort: an unrecorded reading costs the NEXT run a
		// pre-flight, never this one its correctness.
		_ = store.Record(context.Background(), key, r)
	}), nil
}

// LocalUsagePreflight reports whether the machine-wide cap currently blocks
// new work, from what earlier runs in this process observed. Empty reason
// means "go ahead" — including when no cap is configured, or when nothing
// has been measured yet.
func LocalUsagePreflight() (blocked bool, reason string) {
	pol, err := usagecap.FromEnv()
	if err != nil || !pol.Enabled() {
		return false, ""
	}
	readings, err := processUsageStore().Latest(context.Background(), usagecap.Key("", usagecap.ScopeLocal))
	if err != nil {
		return false, ""
	}
	d := usagecap.Preflight(readings, pol, time.Now().UTC(), usagecap.DefaultMaxAge)
	return d.Blocked, d.Reason
}
