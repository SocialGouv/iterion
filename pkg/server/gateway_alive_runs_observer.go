package server

import (
	"context"
	"time"

	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gatewayAliveRunsObserveInterval is how often the per-gateway gauge is
// sampled. One listing of the alive runs per pass: the same set the run
// admission ledger counts, read once and grouped in Go.
const gatewayAliveRunsObserveInterval = time.Minute

// gatewayAliveRunsObserver publishes iterion_gateway_alive_runs{host}: per
// gateway host, the alive subbot trees holding its capacity subject
// (store.CountCapacityHolders over store.AliveCapacityLister). One goroutine
// owns it, so its edge state needs no lock.
type gatewayAliveRunsObserver struct {
	lister  store.AliveCapacityLister
	metrics *cloudmetrics.Registry
	warn    func(format string, args ...any)
	// blind is whether the last pass failed: the warning is written on the
	// rising edge only, the standing condition is the unavailable gauge's.
	blind bool
}

// newGatewayAliveRunsObserver raises the unavailable gauge until a pass
// says otherwise: a host absent from the gauge reads "no alive run" both on
// a quiet gateway and before any reading, and the second must not pass for
// the first.
func newGatewayAliveRunsObserver(lister store.AliveCapacityLister, m *cloudmetrics.Registry, warn func(string, ...any)) *gatewayAliveRunsObserver {
	m.GatewayAliveRunsUnavailable.Set(1)
	return &gatewayAliveRunsObserver{lister: lister, metrics: m, warn: warn}
}

// startGatewayAliveRunsObserver arms the sampler of the per-gateway gauge
// (ADR-167 T2). No election and no durable state: every replica reads the
// same shared listing and publishes the same reading, which is why the
// gauge's help says to aggregate with max. A run store that does not host
// the listing leaves the gauge unavailable, said once at boot.
func (s *Server) startGatewayAliveRunsObserver() {
	m := s.cfg.Metrics
	if m == nil {
		return
	}
	lister := store.AsAliveCapacityLister(s.cfg.Store)
	if lister == nil {
		m.GatewayAliveRunsUnavailable.Set(1)
		s.warnf("gateway alive-runs observer NOT started: the run store does not implement ListAliveRunsForCapacity — " +
			"iterion_gateway_alive_runs publishes nothing and iterion_gateway_alive_runs_unavailable stays at 1")
		return
	}
	o := newGatewayAliveRunsObserver(lister, m, s.warnf)
	s.goUntilShutdown("server.gatewayAliveRunsObserver", func(ctx context.Context) {
		o.run(ctx, gatewayAliveRunsObserveInterval)
	})
}

// run samples until ctx is cancelled: a boot pass, then one per interval, so
// a restart does not leave the gauge empty for a whole interval. Each pass is
// bounded (samplePass): a listing that hangs fails its pass and raises the
// unavailable gauge rather than freezing the per-host values unflagged.
func (o *gatewayAliveRunsObserver) run(ctx context.Context, interval time.Duration) {
	samplePass(ctx, interval, o.observe)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			samplePass(ctx, interval, o.observe)
		}
	}
}

// observe publishes one whole pass: the vector is replaced, so a host whose
// last tree ended loses its series. A failed read replaces nothing — the
// per-host values hold — and raises the unavailable gauge.
func (o *gatewayAliveRunsObserver) observe(ctx context.Context) {
	runs, err := o.lister.ListAliveRunsForCapacity(ctx)
	if err != nil {
		if !o.blind && o.warn != nil {
			o.warn("gateway alive-runs observer: the alive runs could not be listed (%v) — iterion_gateway_alive_runs_unavailable goes to 1 and the per-host series hold their last values until a read succeeds", err)
		}
		o.blind = true
		o.metrics.GatewayAliveRunsUnavailable.Set(1)
		return
	}
	o.blind = false
	values := map[string]float64{}
	for subject, count := range store.CountCapacityHolders(runs, store.GatewayCapacitySubjectPrefix) {
		if host, ok := store.GatewayHostOfSubject(subject); ok {
			values[host] = float64(count.Trees)
		}
	}
	o.metrics.GatewayAliveRuns.Replace(values)
	o.metrics.GatewayAliveRunsUnavailable.Set(0)
}
