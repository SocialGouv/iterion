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
	leaseBoardIssueSync   = "board-issue-sync"
)

// leasedNets is every name above, with what runs on every replica while that
// net is unelected and whether THIS server starts it. The no-shared-database
// warning takes both its trigger and its text from this value, so the message
// can neither name fewer nets than the server starts — a hand-written list
// naming one net while a second one silently runs on every replica is the hole
// that warning exists to close — nor name one it never starts. A net that
// campaigns without an entry here is caught by the package's own sweep over
// this file (leases_class_test.go).
var leasedNets = []struct {
	// name is the lease name, which is what an operator greps in the lease
	// collection.
	name string
	// what is the work that runs on all replicas at once when the net is not
	// elected on one.
	what string
	// wired is the net's OWN start condition, the same expression its start
	// site evaluates (server_lifecycle.go) — one function, two readers, so the
	// warning cannot drift from what actually starts. A nil predicate makes
	// the net always named rather than invisible, and the class sweep rejects
	// it: silence is the failure mode this registry exists to prevent.
	wired func(*Server) bool
}{
	{leaseMergeGateSweeper, "the merge-gate sweep", (*Server).gateSweepStarts},
	{leaseBoardIssueSync, "the forge→board issue sync, whose every pass then lists EVERY sync-enabled repo's issue window once per replica", (*Server).boardIssueSyncStarts},
}

// gateSweepStart is the merge-gate sweep's start condition and the store
// capability it scans with: the run store has to list notifiable runs, and the
// sweep's offers need the publish-token and connection stores to post through.
// Its start site (server_lifecycle.go) and the lease registry above both ask
// HERE — a trigger copied beside a start site is how a net ends up named in a
// warning it never earned, or taking the memory lease store in silence.
func (s *Server) gateSweepStart() (gateSweepLister, bool) {
	lister, ok := s.cfg.Store.(gateSweepLister)
	return lister, ok && s.forgePublishTokens != nil && s.forgeConnections != nil
}

// gateSweepStarts is gateSweepStart's verdict alone, the shape the registry
// takes.
func (s *Server) gateSweepStarts() bool {
	_, ok := s.gateSweepStart()
	return ok
}

// boardIssueSyncStarts is the forge→board issue sync's start condition: a
// cloud board to mirror onto and the integration store naming which repos to
// read. Same two readers as gateSweepStart.
func (s *Server) boardIssueSyncStarts() bool {
	return s.cfg.CloudBoardFor != nil && s.forgeIntegrations != nil
}

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
// The fallback would put the leased nets back on every replica with nothing
// saying so, so a server that starts one warns — and names exactly the ones it
// starts, by the lease names an operator greps, each asked through its own
// start condition (leasedNets.wired). Announcing the fallback on one net's
// signal leaves the others taking it in silence; naming a net this
// configuration never starts sends the operator after work that is not
// running.
//
// It takes the SERVER, not the Config: a net's start condition reads fields
// derived during construction (the publish-token registry the merge-gate sweep
// needs, say), and a Config-shaped copy of those conditions is the thing that
// drifts.
func leaseStoreFor(s *Server, warnf func(format string, args ...any)) lease.Store {
	if s.cfg.Leases != nil {
		return s.cfg.Leases
	}
	if dbs, ok := s.cfg.Store.(interface{ DB() *mongo.Database }); ok && dbs.DB() != nil {
		return lease.NewMongoStore(dbs.DB())
	}
	nets := make([]string, 0, len(leasedNets))
	for _, n := range leasedNets {
		if n.wired == nil || n.wired(s) {
			nets = append(nets, n.name+" ("+n.what+")")
		}
	}
	if len(nets) > 0 && warnf != nil {
		warnf("leases: this configuration wires a leased net and no shared lease store — Config.Leases is unset and the "+
			"run store exposes no database, so every replica elects itself and each leased net it starts runs on all "+
			"of them: %s", strings.Join(nets, "; "))
	}
	return lease.NewMemoryStore()
}
