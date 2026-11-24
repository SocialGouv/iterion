package metrics

import (
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func snapshotValues(t *testing.T, samples []prometheus.Metric) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, sample := range samples {
		var value dto.Metric
		if err := sample.Write(&value); err != nil {
			t.Fatal(err)
		}
		if len(value.Label) != 1 || value.Label[0].GetName() != "pool" {
			t.Fatalf("labels: %v", value.Label)
		}
		out[value.Label[0].GetValue()] = value.GetGauge().GetValue()
	}
	return out
}

func TestGaugeSnapshotCollectionRetainsOneCompletePublication(t *testing.T) {
	g := New().DLQDepthByPool
	old := map[string]float64{"alpha": 3, "beta": 7}
	g.Replace(old)
	ch := make(chan prometheus.Metric)
	go func() { g.Collect(ch); close(ch) }()
	// Receiving one sample proves Collect has captured its snapshot. The
	// second send stays blocked while a new publication completes. Read the
	// metric values only afterwards, as Registry.Gather is allowed to do.
	first := <-ch
	g.Replace(map[string]float64{"alpha": 30, "beta": 70})
	got := []prometheus.Metric{first}
	for sample := range ch {
		got = append(got, sample)
	}
	if values := snapshotValues(t, got); !reflect.DeepEqual(values, old) {
		t.Fatalf("concurrent publication changed retained scrape: %v, want %v", values, old)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(g)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || len(families[0].Metric) != 2 {
		t.Fatalf("fresh sample incomplete: %v", families)
	}
	for _, m := range families[0].Metric {
		want := map[string]float64{"alpha": 30, "beta": 70}[m.Label[0].GetValue()]
		if m.GetGauge().GetValue() != want {
			t.Fatalf("fresh value %v, want %v", m, want)
		}
	}
}

func TestGaugeSnapshotOwnsValuesAndRetiresLabelsTogether(t *testing.T) {
	g := New().DLQDepthByPool
	input := map[string]float64{"alpha": 3, "beta": 7}
	g.Replace(input)
	input["alpha"] = 99
	delete(input, "beta")
	ch := make(chan prometheus.Metric, 2)
	g.Collect(ch)
	close(ch)
	var got []prometheus.Metric
	for sample := range ch {
		got = append(got, sample)
	}
	if values := snapshotValues(t, got); !reflect.DeepEqual(values, map[string]float64{"alpha": 3, "beta": 7}) {
		t.Fatalf("caller changed published sample: %v", values)
	}
	g.Replace(nil)
	reg := prometheus.NewRegistry()
	reg.MustRegister(g)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 0 {
		t.Fatalf("retired labels remain: %v", families)
	}
}
