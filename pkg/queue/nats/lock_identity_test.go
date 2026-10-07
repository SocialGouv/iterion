package nats

// Plan v2 §P5-c: a run lease carries the admitted identity of the run
// it guards — the tenant (and runner pool, when stamped) of the
// message the holder admitted — and the holder refuses to refresh or
// release a lease whose stored identity disagrees with its own.
//
// Gated on ITERION_TEST_NATS_URI like the JetStream integration tests
// (CI: nats-conformance job starts a `nats -js` broker): the KV bucket
// is real, so acquire/refresh/release run against a live broker.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The subjects are package constants, so one broker serves one Conn:
// these witnesses share a single connection (distinct run keys, no
// publishes — only the KV bucket is exercised).
var (
	leaseConnOnce sync.Once
	leaseShared   *Conn
	leaseConnErr  error
)

func leaseConn(t *testing.T) *Conn {
	t.Helper()
	if os.Getenv("ITERION_TEST_NATS_URI") == "" {
		t.Skip("ITERION_TEST_NATS_URI unset — skipping run-lease identity tests (CI: nats-conformance job)")
	}
	leaseConnOnce.Do(func() {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		leaseShared, leaseConnErr = Connect(context.Background(), Config{
			URL:             os.Getenv("ITERION_TEST_NATS_URI"),
			StreamName:      "ITERION_RUNS_TEST_LEASE_" + suffix,
			DLQStream:       "ITERION_RUNS_DLQ_TEST_LEASE_" + suffix,
			KVBucket:        "test-lease-id-" + suffix,
			RolloutKVBucket: "test-lease-rollout-" + suffix,
			ConsumerName:    "test-lease-runners-" + suffix,
			MaxDeliver:      2,
			AckWait:         2 * time.Second,
			MaxAge:          time.Hour,
		})
	})
	if leaseConnErr != nil {
		t.Fatalf("connect: %v", leaseConnErr)
	}
	return leaseShared
}

// forgeLease rewrites the run's stored lease under another admitted
// identity — exactly what a pod that can read the bucket can do: get
// the current revision, CAS-update the body over it.
func forgeLease(t *testing.T, conn *Conn, runID, tenant, pool string) {
	t.Helper()
	entry, err := conn.kv.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("forge get: %v", err)
	}
	body, err := newLeaseBody("runner-evil", store.LeaseIdentity{TenantID: tenant, Pool: pool})
	if err != nil {
		t.Fatalf("forge marshal: %v", err)
	}
	if _, err := conn.kv.Update(context.Background(), runID, body, entry.Revision()); err != nil {
		t.Fatalf("forge update: %v", err)
	}
}

func readLease(t *testing.T, conn *Conn, runID string) LeaseInfo {
	t.Helper()
	entry, err := conn.kv.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("kv get %s: %v", runID, err)
	}
	var info LeaseInfo
	if err := json.Unmarshal(entry.Value(), &info); err != nil {
		t.Fatalf("lease %s: %v", runID, err)
	}
	return info
}

// The lease is attributed on acquire and the honest path is untouched:
// refresh holds, release clears.
func TestRunLeaseCarriesAdmittedIdentity(t *testing.T) {
	conn := leaseConn(t)
	ctx := context.Background()
	runID := fmt.Sprintf("run-lease-id-%d", time.Now().UnixNano())

	// No admitted identity in ctx: the acquire refuses — an
	// unattributable lease must never come to exist.
	if _, err := conn.AcquireLock(ctx, runID, "runner-a"); !errors.Is(err, ErrLeaseUnattributed) {
		t.Fatalf("unattributed acquire: err = %v, want ErrLeaseUnattributed", err)
	}
	if _, err := conn.kv.Get(ctx, runID); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("refused acquire wrote a lease anyway: err = %v", err)
	}

	lock, err := conn.AcquireLock(store.WithLeaseIdentity(ctx, "team-a", "pool-a"), runID, "runner-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	info := readLease(t, conn, runID)
	if info.TenantID != "team-a" || info.Pool != "pool-a" {
		t.Fatalf("lease identity: tenant %q pool %q, want team-a/pool-a", info.TenantID, info.Pool)
	}

	if err := lock.Refresh(ctx); err != nil {
		t.Fatalf("honest refresh: %v", err)
	}
	info = readLease(t, conn, runID)
	if info.TenantID != "team-a" || info.Pool != "pool-a" {
		t.Fatalf("refresh dropped the identity: tenant %q pool %q", info.TenantID, info.Pool)
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("honest release: %v", err)
	}
	if _, err := conn.kv.Get(ctx, runID); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("released lease still present: err = %v, want ErrKeyNotFound", err)
	}
}

// A refresh over a lease rewritten under another admission — same
// tenant with a different pool, or a different tenant — is refused, and
// the stored lease is left exactly as the other admission wrote it.
func TestRunLeaseRefreshRefusesForeignIdentity(t *testing.T) {
	conn := leaseConn(t)
	ctx := context.Background()
	base := fmt.Sprintf("run-lease-refresh-%d", time.Now().UnixNano())

	for _, tc := range []struct {
		name        string
		forgeTenant string
		forgePool   string
	}{
		{"different tenant", "team-b", "pool-a"},
		{"same tenant, different pool", "team-a", "pool-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := base + "-" + tc.forgeTenant + "-" + tc.forgePool
			lock, err := conn.AcquireLock(store.WithLeaseIdentity(ctx, "team-a", "pool-a"), runID, "runner-a")
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			forgeLease(t, conn, runID, tc.forgeTenant, tc.forgePool)

			if err := lock.Refresh(ctx); !errors.Is(err, ErrLeaseIdentityMismatch) {
				t.Fatalf("refresh over foreign lease: err = %v, want ErrLeaseIdentityMismatch", err)
			}
			// The refresh must not have acted: the foreign body survives.
			info := readLease(t, conn, runID)
			if info.RunnerID != "runner-evil" || info.TenantID != tc.forgeTenant || info.Pool != tc.forgePool {
				t.Fatalf("foreign lease was rewritten by the refused refresh: %+v", info)
			}
		})
	}
}

// A lease that evaporated under its holder (TTL expiry) keeps the
// pre-identity semantics: a refresh fails on the CAS alone — no
// identity claim is invented over a key nobody holds — and a release
// of an already-gone lease stays non-fatal.
func TestRunLeaseVanishedKeyKeepsCASSemantics(t *testing.T) {
	conn := leaseConn(t)
	ctx := context.Background()
	runID := fmt.Sprintf("run-lease-vanish-%d", time.Now().UnixNano())

	lock, err := conn.AcquireLock(store.WithLeaseIdentity(ctx, "team-a", "pool-a"), runID, "runner-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := conn.kv.Delete(ctx, runID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := lock.Refresh(ctx); err == nil || errors.Is(err, ErrLeaseIdentityMismatch) {
		t.Fatalf("refresh over vanished lease: err = %v, want the CAS failure without an identity claim", err)
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("release of vanished lease: err = %v, want nil", err)
	}
}

// A release over a foreign lease is refused and the foreign lease
// survives: this holder never deletes a lease it does not own the
// identity of.
func TestRunLeaseReleaseRefusesForeignIdentity(t *testing.T) {
	conn := leaseConn(t)
	ctx := context.Background()
	runID := fmt.Sprintf("run-lease-release-%d", time.Now().UnixNano())

	lock, err := conn.AcquireLock(store.WithLeaseIdentity(ctx, "team-a", "pool-a"), runID, "runner-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	forgeLease(t, conn, runID, "team-b", "pool-b")

	if err := lock.Release(ctx); !errors.Is(err, ErrLeaseIdentityMismatch) {
		t.Fatalf("release of foreign lease: err = %v, want ErrLeaseIdentityMismatch", err)
	}
	info := readLease(t, conn, runID)
	if info.TenantID != "team-b" || info.RunnerID != "runner-evil" {
		t.Fatalf("foreign lease was deleted by the refused release: %+v", info)
	}
}
