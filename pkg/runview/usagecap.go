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
// draws on), else one over the caller's live PolicySource (the DB-backed
// runtime settings), otherwise one built from the machine-wide env policy.
//
// A malformed policy is an error rather than an absent cap: every wrong
// answer here fails open, and a guard silently disabled by a typo is the
// failure the whole package exists to prevent.
func resolveUsageGuard(spec ExecutorSpec) (*usagecap.Guard, error) {
	if spec.UsageGuard != nil {
		return spec.UsageGuard, nil
	}
	store := processUsageStore()
	// No credential fingerprint here, deliberately, and it is not an
	// oversight the cloud path forgot to mirror: the local LLM credential
	// is never a secrets.Credentials entry. ResolveLocalCredentials fills
	// only the workflow's declared GENERIC secrets — the subscription
	// itself is the host's own ~/.claude OAuth dir or env, resolved inside
	// the delegate, invisible at this layer and at the launch gate that
	// must read the SAME key back (usagePreflightFrom). Splitting one side
	// only would make the local pre-flight find nothing, ever — a cap
	// silently disabled, which is worse than the staleness it would fix.
	// The residue: a long-lived studio whose operator rotates their
	// credential reads the replaced account's window until the process
	// restarts (this store is process memory, not Mongo), with the mid-run
	// guard as the backstop.
	key := usagecap.Key("", usagecap.ScopeLocal, "")
	sink := func(r usagecap.Reading) {
		// Best effort: an unrecorded reading costs the NEXT run a
		// pre-flight, never this one its correctness.
		_ = store.Record(context.Background(), key, r)
	}
	if spec.UsageCapSource != nil {
		// A live source gets a guard even when nothing is capped right
		// now — the answer can change (the runtime settings record)
		// before this run ends, and the guard re-reads per evaluation.
		if spec.Logger != nil {
			if pol := spec.UsageCapSource.Effective(context.Background()); pol.Enabled() {
				spec.Logger.Info("usage cap armed: %s", pol)
			}
		}
		return usagecap.NewGuardWithSource(spec.UsageCapSource, sink), nil
	}
	pol, err := usagecap.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("runview: usage cap: %w", err)
	}
	if !pol.Enabled() {
		return nil, nil
	}
	if spec.Logger != nil {
		spec.Logger.Info("usage cap armed: %s", pol)
	}
	return usagecap.NewGuard(pol, sink), nil
}

// LocalUsagePreflight reports whether the machine-wide cap currently blocks
// new work, from what earlier runs in this process observed. Empty reason
// means "go ahead" — including when no cap is configured, or when nothing
// has been measured yet.
func LocalUsagePreflight() (blocked bool, reason string) {
	return usagePreflightFrom(nil)
}

// usagePreflightFrom is LocalUsagePreflight over an optional live policy
// source; nil falls back to the env-resolved policy.
func usagePreflightFrom(src usagecap.PolicySource) (blocked bool, reason string) {
	var pol usagecap.Policy
	if src != nil {
		pol = src.Effective(context.Background())
	} else {
		p, err := usagecap.FromEnv()
		if err != nil {
			return false, ""
		}
		pol = p
	}
	if !pol.Enabled() {
		return false, ""
	}
	readings, err := processUsageStore().Latest(context.Background(), usagecap.Key("", usagecap.ScopeLocal, ""))
	if err != nil {
		return false, ""
	}
	d := usagecap.Preflight(readings, pol, time.Now().UTC(), usagecap.DefaultMaxAge)
	return d.Blocked, d.Reason
}
