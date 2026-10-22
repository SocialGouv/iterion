package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// scrapeMetrics renders the registry the way Prometheus reads it. The label
// set is the thing under test here, and a typed read of one series cannot say
// whether the series the operator queries exists at all.
func scrapeMetrics(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape /metrics: status %d", rec.Code)
	}
	return rec.Body.String()
}

// probeResultValue reads one labelled series of the probe counter.
func probeResultValue(t *testing.T, reg *metrics.Registry, result string) float64 {
	t.Helper()
	c, err := reg.RunsRetryProbes.GetMetricWithLabelValues(result)
	if err != nil {
		t.Fatalf("probe counter for result %q: %v", result, err)
	}
	return counterValue(t, c)
}

func mustScrapeLine(t *testing.T, body, line string) {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == line {
			return
		}
	}
	var got []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "iterion_runs_retry_probes_total") {
			got = append(got, l)
		}
	}
	t.Errorf("the scrape does not carry %q\n  what it carries: %s", line, strings.Join(got, " | "))
}

// TestRetryProbes_TheCounterSaysWhatTheProbeFound is #2450's third point: a
// probe costs a pod, so the operator deciding whether the pass earns its keep
// has to be able to tell a probe that found the window reopened from one that
// found the wall exactly where the provider said it was.
//
// The counter therefore carries the probe's own disposition. `pulled` is the
// pod spent; `stood_down` is a pull whose resume never reached the runner;
// `still_blocked` (counted by the runner, beside the free re-arm) is the
// preflight refusing again. `pulled` minus the other two is what the pass is
// for: the probes whose run got past the preflight.
func TestRetryProbes_TheCounterSaysWhatTheProbeFound(t *testing.T) {
	st := newFakeRetryStore()
	s := newRetrySweeperServer(t, st, nil)
	s.cfg.Metrics = metrics.New()
	now := time.Now().UTC()
	wall := now.Add(4 * 24 * time.Hour)

	s.sweepRetryProbes(context.Background(),
		&fakeRetryLister{probeRefs: []mongostore.RetryDueRef{dueRef("run-parked", wall)}}, now)
	mustScrapeLine(t, scrapeMetrics(t, s.cfg.Metrics), `iterion_runs_retry_probes_total{result="pulled"} 1`)

	// The probe's resume never happened (admission deferred): the sweeper put
	// the announced instant back. That pod bought nothing, and it is not the
	// provider's window that said so.
	st.loadRun["run-probed"] = probedRun("run-probed", now, wall)
	if !s.standDownProbedRetry(context.Background(), st, "team-1", "run-probed", "admission deferred") {
		t.Fatal("the stand-down did not take a probe-marked row")
	}
	body := scrapeMetrics(t, s.cfg.Metrics)
	mustScrapeLine(t, body, `iterion_runs_retry_probes_total{result="stood_down"} 1`)
	mustScrapeLine(t, body, `iterion_runs_retry_probes_total{result="pulled"} 1`)
}
