package server

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/SocialGouv/iterion/pkg/lease"
)

// The lease names of the server's singleton nets (pkg/lease), in one place so
// two nets can never campaign for the same name by accident.
const (
	leaseMergeGateSweeper = "merge-gate-sweeper"
)

// newReplicaID names this process as a lease owner: the host — the pod name
// under Kubernetes, which is what an operator reads in the lease document —
// plus a random suffix, because two processes on one host (a restart
// overlapping its predecessor, two servers in one test binary) must never
// present the same owner.
func newReplicaID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "iterion"
	}
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail (Go ≥ 1.24)
	return strings.TrimSpace(host) + "-" + hex.EncodeToString(b[:])
}

// leaseStoreFor picks where the singleton nets campaign: Config.Leases when the
// caller supplied one; else the Mongo store on the cloud run store's database,
// the one store every replica shares; else an in-memory store, which elects
// every process that uses it — right for a single process, and the bug itself
// on N replicas.
//
// A run store that lists runs for the merge-gate sweep is a cloud store. If it
// offers no database, the fallback would put the sweep back on every replica
// with nothing saying so, so that case warns.
func leaseStoreFor(cfg Config, warnf func(format string, args ...any)) lease.Store {
	if cfg.Leases != nil {
		return cfg.Leases
	}
	if dbs, ok := cfg.Store.(interface{ DB() *mongo.Database }); ok && dbs.DB() != nil {
		return lease.NewMongoStore(dbs.DB())
	}
	if _, cloud := cfg.Store.(gateSweepLister); cloud && warnf != nil {
		warnf("leases: the run store lists runs for the merge-gate sweep but exposes no shared database — " +
			"every replica elects itself, and the sweep runs on each of them")
	}
	return lease.NewMemoryStore()
}
