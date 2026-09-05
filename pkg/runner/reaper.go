package runner

import (
	"context"
	"os"
	"time"

	k8ssandbox "github.com/SocialGouv/iterion/pkg/sandbox/kubernetes"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The k8s sandbox reaper, wired into the RUNNER (not the server). See
// ADR-070 + the "managed-cloud residual" it closes.
//
// PR #120 shipped ReapOrphanResources but wired it only into
// runview.Service, gated on Capabilities().CrossProcessLock — false on
// the managed-cloud (lock-less Mongo) server, and the cloud runner
// never constructs a runview.Service. So in managed cloud the reaper
// never fired.
//
// The ownerReference → runner-pod cascade only GCs the sandbox pod +
// its plaintext-credential Secret when the runner POD OBJECT is deleted
// (rollout / drain / scale-down). A container OOM/SIGKILL restarts the
// container in place — the pod UID survives, nothing cascades. The
// cloud runner is a long-lived Deployment, so OOM-with-surviving-pod is
// the COMMON failure, not the rare one, and the Secret leaks until the
// next rollout (the #115 scenario).
//
// The runner is in-cluster AND has real liveness authority (its NATS
// KV lease), so it is the right home for the periodic reap. A healthy
// SIBLING runner reaps a dead runner's orphaned sandbox + Secrets +
// NetworkPolicy within one tick — exactly what the namespace-wide
// managed-resource list was built for.

// defaultSandboxReapInterval is how often the runner re-runs the k8s
// sandbox reap after its boot scan. Overridable via
// ITERION_SANDBOX_REAP_INTERVAL (a Go duration; "0" disables the
// ticker, keeping only the boot-time reap).
const defaultSandboxReapInterval = 60 * time.Second

func sandboxReapInterval() time.Duration {
	v := os.Getenv("ITERION_SANDBOX_REAP_INTERVAL")
	if v == "" {
		return defaultSandboxReapInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultSandboxReapInterval
	}
	return d // <= 0 disables the ticker (boot scan still runs)
}

// runLeaseChecker reports whether a runner currently holds the run's
// NATS KV lease. Implemented by *natsq.Conn (the same mechanism the
// queue sweeper trusts). Abstracted so the reap predicate is unit
// testable without a live NATS connection.
type runLeaseChecker interface {
	IsRunLocked(ctx context.Context, runID string) (bool, error)
}

// runLoader is the slice of store.RunStore the reap predicate's
// store-status backstop needs. Abstracted for the same reason.
type runLoader interface {
	LoadRun(ctx context.Context, runID string) (*store.Run, error)
}

// runSandboxReaper reaps orphaned k8s sandbox resources at boot and on
// a ticker for as long as the runner loop lives. No-op when the runner
// is not in-cluster (kubernetes.Detect fails) or has no NATS connection
// (the lease is the liveness authority). Started by Run.
//
// Also a no-op when this runner is itself the isolation boundary
// (ITERION_SANDBOX_OVERRIDE=none): that CLI-strength override forbids any
// workflow from activating the k8s sandbox driver, so the runner never
// spawns a sibling sandbox pod — there is nothing to reap, and firing the
// reaper would only surface a permission error (the runner SA isn't granted
// pods RBAC when the sandbox is disabled) on every tick.
func (r *Runner) runSandboxReaper(ctx context.Context) {
	if r.cfg.SandboxOverride == "none" {
		r.cfg.Logger.Info("runner: k8s sandbox reaper disabled — runner is the isolation boundary (ITERION_SANDBOX_OVERRIDE=none), no sandbox pods to reap")
		return
	}
	interval := sandboxReapInterval()
	// Boot-time reap: catch a sibling that died before THIS runner booted.
	r.reapOrphanSandboxResources(ctx)
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reapOrphanSandboxResources(ctx)
		}
	}
}

// reapOrphanSandboxResources performs one namespace-wide reap pass.
// Guarded on being in-cluster (kubernetes.Detect) and on having a lease
// authority (NATS). The reap predicate is liveness-first (NATS lease),
// with a store-status backstop — see sandboxResourceReapable.
func (r *Runner) reapOrphanSandboxResources(ctx context.Context) {
	if r.cfg.NATS == nil {
		return // no lease authority — never reap without one
	}
	_, namespace, err := k8ssandbox.Detect()
	if err != nil {
		return // not an in-cluster runner — nothing to reap
	}
	// Namespace-wide admin scan: the LoadRun backstop must see runs
	// across tenants, so drop the tenant filter (the KV lease check is
	// tenant-agnostic already).
	scanCtx := store.WithoutTenantFilter(ctx)
	reaped, err := k8ssandbox.ReapOrphanResources(scanCtx, namespace, func(runID string) bool {
		return r.sandboxResourceReapable(scanCtx, runID)
	})
	if err != nil {
		r.cfg.Logger.Warn("runner: reap orphan k8s sandbox resources: %v", err)
	}
	if len(reaped) > 0 {
		r.cfg.Logger.Info("runner: reaped %d orphan k8s sandbox resource(s)", len(reaped))
	}
}

// sandboxResourceReapable is the isTerminal predicate for the runner's
// k8s reap. Delegates to the pure sandboxResourceReapable so the
// liveness/backstop logic is unit testable without a Runner.
func (r *Runner) sandboxResourceReapable(ctx context.Context, runID string) bool {
	return sandboxResourceReapable(ctx, r.cfg.NATS, r.cfg.Store, runID)
}

// sandboxResourceReapable reports whether a managed sandbox resource
// (pod / Secret / NetworkPolicy) whose owning run is runID may be
// reaped.
//
// Liveness FIRST — the NATS lease is the authority (the Mongo store is
// lock-less, so store.LockRun cannot prove liveness here; the queue
// sweeper trusts the exact same IsRunLocked signal):
//   - no run owner (empty label)         → reap (a managed resource with
//     no run-id label is an orphan by construction).
//   - lease HELD by any runner           → SKIP (a run still claimed by
//     any sibling is in flight — never reap its sandbox).
//   - lease-check ERRORED                → SKIP (unknown liveness →
//     fail safe; retry next tick).
//
// Then a store-status BACKSTOP (the lease is already known absent):
//   - run record provably absent          → reap (terminal-or-gone).
//   - status terminal                     → reap.
//   - status running / paused             → SKIP (the store still thinks
//     the run is active; the lease is momentarily absent — e.g. between
//     claim and first heartbeat, or a brief KV blip. Reap only when
//     BOTH signals agree the run is dead; the queue sweeper flips a
//     genuinely-orphaned stale-running row to failed_resumable, and the
//     next tick then reaps it).
//   - store LOOKUP ERRORED (transient)     → SKIP (unknown store status
//     is NOT proof of death — a store outage / decode failure / context
//     deadline must never be read as "the run is gone", or a live run's
//     sandbox + credential Secret would be force-deleted mid-flight on a
//     store blip. Only a provable absence — store.RunAbsent: never
//     existed, or deleted and tombstoned — is absence; every other error
//     fails safe like the lease check does).
func sandboxResourceReapable(ctx context.Context, leases runLeaseChecker, loader runLoader, runID string) bool {
	if runID == "" {
		return true // managed resource with no run owner → orphan
	}
	locked, err := leases.IsRunLocked(ctx, runID)
	if err != nil {
		return false // liveness unknown → fail safe, keep the resource
	}
	if locked {
		return false // lease held → a runner owns it → in flight, keep it
	}
	run, err := loader.LoadRun(ctx, runID)
	if err != nil {
		if store.RunAbsent(err) {
			return true // no lease + provably gone from store (never existed, or deleted and tombstoned) → orphan, reap
		}
		return false // transient store error → unknown status → fail safe, keep
	}
	return run.Status.IsTerminal()
}
