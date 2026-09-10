package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func probe(t *testing.T, h http.Handler) (int, healthBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var body healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode probe body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

// The whole point of replacing the tcpSocket probe: tell a BUSY runner
// (loop legitimately blocked inside processOne, possibly for hours) from
// a WEDGED one (loop stopped cycling with nothing to show for it). Get
// this backwards and the kubelet kills pods mid-run.
func TestHealthAliveDistinguishesBusyFromWedged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		h    Health
		want bool
	}{
		{"booting", Health{}, true},
		{"idle and cycling", Health{Started: true, SinceLastTick: 3 * time.Second}, true},
		{"busy for hours", Health{Started: true, Busy: true, SinceLastTick: 6 * time.Hour}, true},
		{"idle and wedged", Health{Started: true, SinceLastTick: livenessStaleFloor + time.Second}, false},
		{"draining but alive", Health{Started: true, Draining: true, SinceLastTick: time.Second}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.h.Alive(); got != c.want {
				t.Errorf("Alive() = %v, want %v", got, c.want)
			}
		})
	}
}

// The wedge threshold has to scale with the loop's own cadence. An
// operator who raises FetchWait to cut NATS load would otherwise shrink
// the margin from ~17× to ~4× a healthy interval — turning a legitimate
// long-poll into a kubelet restart.
func TestLivenessStaleAfterScalesWithFetchWait(t *testing.T) {
	t.Parallel()

	fast := &Runner{cfg: Config{FetchWait: 5 * time.Second}}
	if got := fast.livenessStaleAfter(); got != livenessStaleFloor {
		t.Errorf("FetchWait=5s → %v, want the %v floor", got, livenessStaleFloor)
	}

	slow := &Runner{cfg: Config{FetchWait: 30 * time.Second}}
	if got := slow.livenessStaleAfter(); got != 10*time.Minute {
		t.Errorf("FetchWait=30s → %v, want 20× = 10m", got)
	}
	// A loop still cycling at its (slow) cadence is alive.
	h := Health{Started: true, SinceLastTick: 5 * time.Minute, StaleAfter: slow.livenessStaleAfter()}
	if !h.Alive() {
		t.Error("a 30s-cadence loop that ticked 5m ago reads as wedged")
	}
}

// Readiness is the honest signal for an operator and a rollout: a pod in
// a lame-duck drain (up to DrainTimeout — 8h) must not read as a fresh
// one taking work.
func TestHealthReadyFalseWhileDraining(t *testing.T) {
	t.Parallel()

	if (Health{}).Ready() {
		t.Error("a runner that has not started reports ready")
	}
	if !(Health{Started: true}).Ready() {
		t.Error("a started, non-draining runner reports not-ready")
	}
	if (Health{Started: true, Draining: true, Busy: true}).Ready() {
		t.Error("a draining runner still reports ready — a rollout cannot tell it is leaving")
	}
}

func TestHealthProviderReportsSupersededEpoch(t *testing.T) {
	t.Parallel()
	p := &HealthProvider{}
	p.Set(func() Health {
		return Health{Superseded: true, Epoch: 4, HighWaterEpoch: 6}
	})
	code, body := probe(t, p.ReadinessHandler())
	if code != http.StatusServiceUnavailable || body.Status != "superseded" {
		t.Fatalf("superseded /readyz = %d %q, want 503 superseded", code, body.Status)
	}
	if body.Epoch != 4 || body.HighWaterEpoch != 6 {
		t.Fatalf("probe epochs = %d/%d, want 4/6", body.Epoch, body.HighWaterEpoch)
	}
	if liveCode, _ := probe(t, p.LivenessHandler()); liveCode != http.StatusOK {
		t.Fatalf("superseded /healthz = %d, want 200", liveCode)
	}
}

// The metrics listener binds before runner.New (a port conflict must
// surface at boot). Until the health source is published the probes must
// say "starting" — not lie with a 200 on readiness, which is exactly the
// window where a pod would be handed work it cannot take.
func TestHealthProviderBeforeSetSaysStarting(t *testing.T) {
	t.Parallel()

	p := &HealthProvider{}

	if code, body := probe(t, p.ReadinessHandler()); code != http.StatusServiceUnavailable || body.Status != "starting" {
		t.Errorf("pre-Set /readyz = %d %q, want 503 starting", code, body.Status)
	}
	// Liveness stays green: the process is booting, not wedged — a 503
	// here would restart-loop a pod that is merely connecting to Mongo.
	if code, body := probe(t, p.LivenessHandler()); code != http.StatusOK || body.Status != "starting" {
		t.Errorf("pre-Set /healthz = %d %q, want 200 starting", code, body.Status)
	}

	live := Health{Started: true, SinceLastTick: time.Second}
	p.Set(func() Health { return live })
	if code, _ := probe(t, p.ReadinessHandler()); code != http.StatusOK {
		t.Errorf("post-Set /readyz = %d, want 200", code)
	}
}

func TestHealthProviderReportsStalledAndDraining(t *testing.T) {
	t.Parallel()

	h := Health{Started: true, SinceLastTick: livenessStaleFloor + time.Minute}
	p := &HealthProvider{}
	p.Set(func() Health { return h })

	code, body := probe(t, p.LivenessHandler())
	if code != http.StatusServiceUnavailable || body.Status != "stalled" {
		t.Errorf("wedged /healthz = %d %q, want 503 stalled", code, body.Status)
	}
	if body.IdleFor == "" {
		t.Error("stalled probe body does not say how long the loop has been idle")
	}

	h = Health{Started: true, Draining: true, Busy: true, SinceLastTick: time.Hour}
	code, body = probe(t, p.ReadinessHandler())
	if code != http.StatusServiceUnavailable || body.Status != "draining" {
		t.Errorf("draining /readyz = %d %q, want 503 draining", code, body.Status)
	}
	if !body.Busy {
		t.Error("draining probe body hides the in-flight run")
	}
	// A draining pod is still alive: it is finishing its run, and killing
	// it is exactly what the lame-duck exists to prevent.
	if liveCode, _ := probe(t, p.LivenessHandler()); liveCode != http.StatusOK {
		t.Errorf("draining /healthz = %d, want 200", liveCode)
	}
}

// Health() must read the runner's real state, not a snapshot taken at
// construction — the probe is worthless if it cannot see the flip.
func TestRunnerHealthTracksState(t *testing.T) {
	t.Parallel()

	r := &Runner{}
	if h := r.Health(); h.Started || h.Draining || h.Busy {
		t.Errorf("zero runner = %+v, want nothing started", h)
	}

	// The loop's first tick IS the started signal — there is no second
	// flag that could disagree with it.
	r.tick()
	h := r.Health()
	if !h.Started || h.SinceLastTick > time.Minute {
		t.Errorf("after tick = %+v, want started with a fresh tick", h)
	}

	r.current = &inFlight{runID: "run-1"}
	if !r.Health().Busy {
		t.Error("an in-flight run does not show as busy")
	}

	r.draining.Store(true)
	if !r.Health().Draining {
		t.Error("draining flag not visible through Health()")
	}
}
