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
// A guard is ALWAYS returned, cap or no cap. Recording is not enforcing,
// and the guard is the only path either travels: a host that configured no
// ceiling still needs the provider's refusals on the process ledger — a
// rejected credential, a fair-usage refusal — because that is what the
// pre-flight and the credential-tier skips read. An unconfigured policy is
// simply inert (evaluate returns no decision), so the guard observes,
// publishes, and blocks nothing.
//
// A malformed policy is an error rather than an absent cap: every wrong
// answer here fails open, and a guard silently disabled by a typo is the
// failure the whole package exists to prevent.
//
// The local key carries NO credential fingerprint, unlike the runner's
// (usagecap.Key's third segment) — deliberately, and not merely because
// ExecutorSpec has no credential handle here. Locally there is no connect
// event to stamp a stable identity at: the credential is whatever
// ~/.claude/.credentials.json holds, and the CLI REWRITES that file on
// every token refresh. Hashing it would open a new meter every few hours,
// so no reading would ever accumulate and the cap would go quietly inert —
// strictly worse than the slot-shaped meter, whose only cost is that an
// operator who rotates their own credential mid-process reads the replaced
// account's window until it resets. Closing this properly needs a stable
// account identity, the same one secrets.SubscriptionFingerprint documents
// as missing from Anthropic's payload.
func resolveUsageGuard(spec ExecutorSpec) (*usagecap.Guard, error) {
	if spec.UsageGuard != nil {
		return spec.UsageGuard, nil
	}
	store := processUsageStore()
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
	if spec.Logger != nil && pol.Enabled() {
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
	// A malformed trust window already refused the process at startup
	// (FromEnv validates it); here it fails open like every other
	// uncertainty of this pre-flight.
	trust, err := usagecap.TrustFromEnv()
	if err != nil {
		return false, ""
	}
	d := usagecap.Preflight(readings, pol, time.Now().UTC(), trust)
	return d.Blocked, d.Reason
}
