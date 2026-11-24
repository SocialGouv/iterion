package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/connector/intake"
)

// EnvConnectorWorkerInterval is how often the connector worker ticks (a Go
// duration): one matching pass then one launching pass.
const EnvConnectorWorkerInterval = "ITERION_CONNECTOR_WORKER_INTERVAL"

// defaultConnectorWorkerInterval is the tick when the dial is unset.
const defaultConnectorWorkerInterval = 2 * time.Second

// startConnectorWorker runs the connector intake worker (ADR-168 §8) for
// the server's life. Every replica runs one: the store's claims keep two
// workers off one row, and its generations make a late worker's write fail
// instead of land. Off when the server holds no intake store, no
// subscription store or no run service — there is nothing to drain or
// nothing to launch with. A dial that does not parse keeps it off, said in
// the log: the operator stated a bound this build will not guess at.
func (s *Server) startConnectorWorker() {
	if s.connectorIntakes == nil || s.cfg.TriggerStore == nil || s.runs == nil {
		return
	}
	w, every, err := s.connectorWorker(os.Getenv)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("connectors: the intake worker is off: %v", err)
		}
		return
	}
	s.goUntilShutdown("server.connectorWorker", func(ctx context.Context) {
		if err := w.Run(ctx, every); err != nil && ctx.Err() == nil && s.logger != nil {
			s.logger.Error("connectors: the intake worker stopped: %v", err)
		}
	})
}

// connectorWorker builds the worker over the server's stores, its policies
// and tick read from the dials.
func (s *Server) connectorWorker(getenv func(string) string) (*intake.Worker, time.Duration, error) {
	pairs, err := intake.PolicyFromEnv(intake.EnvPairPrefix, getenv)
	if err != nil {
		return nil, 0, err
	}
	intakes, err := intake.PolicyFromEnv(intake.EnvIntakePrefix, getenv)
	if err != nil {
		return nil, 0, err
	}
	every := defaultConnectorWorkerInterval
	if v := strings.TrimSpace(getenv(EnvConnectorWorkerInterval)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, 0, fmt.Errorf("%s=%q: want a positive Go duration", EnvConnectorWorkerInterval, v)
		}
		every = d
	}
	host, _ := os.Hostname()
	return &intake.Worker{
		Store:     s.connectorIntakes,
		Matcher:   s.connectorMatcher(),
		Submitter: s.connectorSubmitter(),
		Owner:     fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.NewString()[:8]),
		Intakes:   intakes,
		Pairs:     pairs,
		Logger:    s.logger,
	}, every, nil
}
