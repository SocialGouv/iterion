package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/polling"
	"github.com/SocialGouv/iterion/pkg/lease"
)

// connectorPollersStart is the pollers' start condition (ADR-168 §7): the
// poller and intake stores, the subscriptions, and the connection plane a
// poller resolves its credential through. The lease registry reads it too.
func (s *Server) connectorPollersStart() bool {
	return s.connectorPollers != nil && s.connectorIntakes != nil && s.cfg.TriggerStore != nil &&
		s.connectionStore != nil && s.connectorCatalog != nil && s.sealer != nil
}

// connectorPollersOff says why this server runs no pollers, or "" when it
// does — the one predicate the save of a polled subscription reads, so a
// subscription is never accepted to sit on a poller nothing ticks: the start
// condition, and the dials the worker reads, which keep it off when one does
// not parse.
func (s *Server) connectorPollersOff() string {
	if !s.connectorPollersStart() {
		return "this server runs no pollers (no poller store, intake store, subscription store or connection plane)"
	}
	if _, err := polling.PolicyFromEnv(os.Getenv); err != nil {
		return fmt.Sprintf("the pollers are off: %v", err)
	}
	return ""
}

// startConnectorPollers runs the elected poller net for the server's life:
// one replica ticks the due pollers, and each poller's generation keeps a
// holder that overran its lease from writing over its successor. A dial that
// does not parse keeps it off, said in the log: the operator stated a bound
// this build will not guess at.
func (s *Server) startConnectorPollers() {
	if !s.connectorPollersStart() {
		return
	}
	w, err := s.connectorPollerWorker(os.Getenv)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("connectors: the pollers are off: %v", err)
		}
		return
	}
	s.goUntilShutdown("server.connectorPollers", func(ctx context.Context) {
		s.runElectedConnectorPollers(ctx, w)
	})
}

// runElectedConnectorPollers is the lease campaign around the poller passes.
// The election is inside, not around, so there is no unelected entry point
// to start by mistake.
func (s *Server) runElectedConnectorPollers(ctx context.Context, w *polling.Worker) {
	ttl := max(3*w.Policy.Pass, 30*time.Second)
	// A holder whose passes keep failing to reach the vendors it calls hands
	// the term over instead of renewing it while no peer covers (#2685).
	health := s.newLeaseHealth()
	err := lease.Run(ctx, s.leases, lease.Spec{
		Name:   leaseConnectorPollers,
		Owner:  s.replicaID,
		TTL:    ttl,
		Retry:  w.Policy.Pass,
		Info:   s.infof,
		Warn:   s.warnf,
		Health: health,
	}, func(term context.Context) {
		if err := w.Run(term, health.Observe); err != nil && term.Err() == nil {
			s.warnf("connectors: the pollers stopped: %v", err)
		}
	})
	if err != nil && ctx.Err() == nil {
		s.warnf("connectors: the poller lease campaign ended: %v", err)
	}
}

// connectorPollerWorker builds the worker over the server's stores and its
// tier's guarded client — strict in cloud, the operator's hatch locally — the
// same client the connector facade calls on.
func (s *Server) connectorPollerWorker(getenv func(string) string) (*polling.Worker, error) {
	policy, err := polling.PolicyFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	client := connection.CloudHTTPClient()
	if !strings.EqualFold(s.cfg.Mode, "cloud") {
		client = connection.LocalHTTPClient()
	}
	w := &polling.Worker{
		Pollers:       s.connectorPollers,
		Intakes:       s.connectorIntakes,
		Subscriptions: s.cfg.TriggerStore,
		Catalog:       s.connectorCatalogFor,
		Resolver: func(tenantID string) *connection.Resolver {
			// The tenant's own composition — the catalog the subscription was
			// judged against — and its connections; the worker states the
			// trust (connection.TrustOfTenantConfiguration).
			return &connection.Resolver{
				Catalog:  s.connectorCatalogFor(tenantID),
				Store:    s.connectionStore,
				Sealer:   s.sealer,
				TenantID: tenantID,
			}
		},
		Client: client,
		Owner:  s.replicaID,
		Policy: policy,
		Logger: s.logger,
	}
	return w, w.Validate()
}
