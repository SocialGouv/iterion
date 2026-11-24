package server

import (
	"context"
	"errors"
	"reflect"
	"testing"

	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func poolDLQSample(t *testing.T, metrics *cloudmetrics.Registry) map[string]float64 {
	t.Helper()
	// Gather the exposed series, not WithLabelValues: that accessor would itself
	// create a missing label and disguise the producer's missing-label defect.
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.DLQDepthByPool)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "iterion_dlq_depth_by_pool" {
			t.Fatalf("unexpected family %q", f.GetName())
		}
		for _, m := range f.Metric {
			if len(m.Label) != 1 || m.Label[0].GetName() != "pool" {
				t.Fatalf("pool labels = %v", m.Label)
			}
			got[m.Label[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	return got
}

func TestDLQDepthCarriesThePoolLabel(t *testing.T) {
	ctx := context.Background()
	reg := platformcfg.NewMemoryStore[platformcfg.RunnerPools]()
	pools := platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{
		{Name: "alpha", State: platformcfg.RunnerPoolActive},
		{Name: "beta", State: platformcfg.RunnerPoolDraining},
		{Name: "retired", State: platformcfg.RunnerPoolDisabled},
	}}
	if err := reg.Put(ctx, pools); err != nil {
		t.Fatal(err)
	}
	backend := &fakeDLQGaugeBackend{sharedDepth: 5, poolDepth: map[string]uint64{"alpha": 3, "beta": 7, "retired": 99}}
	s := &Server{queue: backend, runnerPoolsStore: reg}
	s.cfg.Metrics = cloudmetrics.New()
	s.updateDLQGauges(ctx)
	want := map[string]float64{"alpha": 3, "beta": 7}
	if got := poolDLQSample(t, s.cfg.Metrics); !reflect.DeepEqual(got, want) {
		t.Fatalf("labels/depths = %v, want %v", got, want)
	}
	if got := dlqGaugeMetric(t, s.cfg.Metrics.DLQDepth); got != 15 {
		t.Fatalf("historical shared + pools sum = %v, want 15", got)
	}

	// A partial sample must not publish zeros or discard a previous label.
	backend.sharedDepth = 0
	backend.poolErr = errors.New("pool unavailable")
	s.updateDLQGauges(ctx)
	if got := poolDLQSample(t, s.cfg.Metrics); !reflect.DeepEqual(got, want) {
		t.Fatalf("unreadable pool changed sample: %v", got)
	}
	if got := dlqGaugeMetric(t, s.cfg.Metrics.DLQDepth); got != 15 {
		t.Fatalf("unreadable pool changed sum: %v", got)
	}
	s.runnerPoolsStore = errorRunnerPoolsStore{}
	s.updateDLQGauges(ctx)
	if got := poolDLQSample(t, s.cfg.Metrics); !reflect.DeepEqual(got, want) {
		t.Fatalf("unreadable registry changed sample: %v", got)
	}

	// Only a successful registry sample can retire a series. Real zero remains
	// present for an active pool; removing all pools removes all pool labels.
	s.runnerPoolsStore = reg
	backend.poolErr = nil
	backend.poolDepth["alpha"] = 0
	pools.Pools = pools.Pools[:1]
	if err := reg.Put(ctx, pools); err != nil {
		t.Fatal(err)
	}
	s.updateDLQGauges(ctx)
	if got := poolDLQSample(t, s.cfg.Metrics); !reflect.DeepEqual(got, map[string]float64{"alpha": 0}) {
		t.Fatalf("retirement/zero = %v", got)
	}
	if err := reg.Put(ctx, platformcfg.RunnerPools{}); err != nil {
		t.Fatal(err)
	}
	s.updateDLQGauges(ctx)
	if got := poolDLQSample(t, s.cfg.Metrics); len(got) != 0 {
		t.Fatalf("retired labels remain: %v", got)
	}
}

func dlqGaugeMetric(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	var metric dto.Metric
	if err := gauge.Write(&metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetGauge().GetValue()
}
