package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
)

// orgPurgeLeasePace is how often a replica that does not hold the org purge
// lease campaigns for it, and the holder's renewal pace. The purge is nightly:
// a dead holder replaced within minutes loses nothing.
const orgPurgeLeasePace = time.Minute

// orgPurgeLeaseTTLFactor is the TTL in paces, like the other elected sweeps.
const orgPurgeLeaseTTLFactor = 3

// orgPurgeStarts is the org purge sweeper's start condition
// (server_lifecycle.go): cloud mode wires one. Its start site and the lease
// registry (leasedNets) both ask HERE.
func (s *Server) orgPurgeStarts() bool {
	return s.cfg.OrgPurgeSweeper != nil
}

// runElectedOrgPurge runs the nightly org purge on the ONE replica holding its
// lease, and campaigns for it on every other. The purge is idempotent — a
// deleted run is tombstoned and not listed again, a gone org is a no-op — but
// its cost is not: every pass lists and deletes the S3 objects of each run of
// every org past its grace, and the run batch (ITERION_ORG_PURGE_RUN_BATCH)
// bounds one pass, not N replicas running it at once. Each term opens with a
// sweep (Sweeper.Run's catch-up), so a holder that dies is replaced by one
// that sweeps at once; a term that ends cancels the purge under way at its
// next run. The term also ends on the WORK failing (lease.Health): a holder
// whose sweeps keep failing on a transport error — its egress to the bucket
// or the database broken — hands the lease to a peer.
func (s *Server) runElectedOrgPurge(ctx context.Context) {
	spec := s.orgPurgeLeaseSpec()
	err := lease.Run(ctx, s.leases, spec, func(term context.Context) {
		s.cfg.OrgPurgeSweeper.Run(term, spec.Health)
	})
	if err != nil {
		s.warnf("org purge sweeper NOT started: %v — soft-deleted organizations stay pending_deletion past their grace, their data and objects in place", err)
	}
}

// orgPurgeLeaseSpec is the org purge's lease.
func (s *Server) orgPurgeLeaseSpec() lease.Spec {
	return lease.Spec{
		Name:   leaseOrgPurgeSweeper,
		Owner:  s.replicaID,
		TTL:    orgPurgeLeaseTTLFactor * orgPurgeLeasePace,
		Retry:  orgPurgeLeasePace,
		Info:   s.infof,
		Warn:   s.warnf,
		Health: s.newLeaseHealth(),
	}
}
