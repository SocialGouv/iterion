package runner

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

const (
	// livenessStaleFloor is the minimum age a silent consume loop must
	// reach before /healthz calls the pod wedged.
	livenessStaleFloor = 2 * time.Minute
	// livenessStaleFetchWaits scales that floor with the loop's own
	// cadence: an IDLE loop ticks every FetchWait, so the threshold has to
	// stay a wide multiple of it. Hardcoding 2min alone would silently
	// shrink the margin to 4× if an operator raised FetchWait to 30s to
	// cut NATS load — turning a healthy long-poll into a kubelet restart.
	livenessStaleFetchWaits = 20
)

// livenessStaleAfter is how long the consume loop may go without a tick
// before /healthz reports the pod as wedged: the floor, or 20× the fetch
// cadence, whichever is longer. At the default FetchWait (5s) that is
// ~17× the healthy interval — only a genuinely stuck loop trips it.
//
// A BUSY runner never trips it at all (see Health.Busy): the loop is
// supposed to be blocked inside processOne for the whole run, which is
// hours by design. Killing that pod would throw away work the engine is
// checkpointing — the drain ceiling and the budget are what bound a run,
// not the kubelet.
func (r *Runner) livenessStaleAfter() time.Duration {
	if scaled := livenessStaleFetchWaits * r.cfg.FetchWait; scaled > livenessStaleFloor {
		return scaled
	}
	return livenessStaleFloor
}

// Health is the runner's probe state. It is what separates "this pod is
// busy" from "this pod is wedged" — a distinction a TCP probe on the
// metrics port cannot make, which is how a runner with a dead consumer
// used to stay Ready forever while consuming nothing.
type Health struct {
	// Started reports that Run has entered its consume loop.
	Started bool `json:"started"`
	// Draining reports that Shutdown has begun (lame-duck or interrupt).
	Draining bool `json:"draining"`
	// Busy reports that a run is being processed right now.
	Busy bool `json:"busy"`
	// Superseded means the pod's literal epoch is below the durable fleet
	// high-water mark. It remains live for diagnosis but can never be ready.
	Superseded     bool   `json:"superseded"`
	Epoch          uint64 `json:"epoch"`
	HighWaterEpoch uint64 `json:"high_water_epoch"`
	// SinceLastTick is the age of the last consume-loop iteration.
	// Meaningless while Busy (the loop is blocked on the run by design).
	SinceLastTick time.Duration `json:"-"`
	// StaleAfter is the age at which a silent idle loop counts as wedged.
	// Carried on the snapshot rather than read from a package constant so
	// it tracks the runner's own fetch cadence. Zero falls back to the
	// floor.
	StaleAfter time.Duration `json:"-"`
}

// Health snapshots the runner's probe state.
func (r *Runner) Health() Health {
	r.mu.Lock()
	busy := r.current != nil
	r.mu.Unlock()

	h := Health{
		Draining:       r.draining.Load(),
		Busy:           busy,
		Superseded:     r.cfg.Superseded,
		Epoch:          r.cfg.RunnerEpoch,
		HighWaterEpoch: r.cfg.HighWaterEpoch,
		StaleAfter:     r.livenessStaleAfter(),
	}
	// Run stamps a tick before anything else, so a non-zero tick IS the
	// started signal — no second flag to keep in sync with it.
	if ns := r.lastTick.Load(); ns > 0 {
		h.Started = true
		h.SinceLastTick = time.Since(time.Unix(0, ns))
	}
	return h
}

// Alive reports whether the consume loop is doing its job: processing a
// run, or cycling through fetches recently enough.
func (h Health) Alive() bool {
	if !h.Started || h.Busy {
		return true // booting, or legitimately blocked on a run
	}
	staleAfter := h.StaleAfter
	if staleAfter <= 0 {
		staleAfter = livenessStaleFloor
	}
	return h.SinceLastTick < staleAfter
}

// Ready reports whether the pod is available to take new work. A draining
// runner answers false for the whole (up to DrainTimeout) lame-duck, which
// is what makes `kubectl get pods` distinguish a pod finishing its run
// from a fresh one.
func (h Health) Ready() bool { return h.Started && !h.Draining && !h.Superseded }

// tick records a consume-loop iteration for the liveness probe.
func (r *Runner) tick() { r.lastTick.Store(time.Now().UnixNano()) }

// HealthProvider is what the probe handler reads. It exists so the
// entrypoint can bind its metrics listener BEFORE the runner is built (a
// port conflict must surface at boot, not later) and still serve honest
// probes in between: an unset provider answers "starting", the runner
// equivalent of a pod that has not signalled ready yet.
type HealthProvider struct {
	fn atomic.Pointer[func() Health]
}

// Set publishes the live health source. Called once, after runner.New.
func (p *HealthProvider) Set(fn func() Health) { p.fn.Store(&fn) }

func (p *HealthProvider) get() (Health, bool) {
	// A stored-but-nil func would panic on every probe, which the kubelet
	// reads as three timeouts and a SIGKILL — so an unusable source is
	// treated exactly like an absent one: "starting".
	if fn := p.fn.Load(); fn != nil && *fn != nil {
		return (*fn)(), true
	}
	return Health{}, false
}

type healthBody struct {
	Status         string `json:"status"`
	Version        string `json:"version,omitempty"`
	Commit         string `json:"commit,omitempty"`
	Busy           bool   `json:"busy"`
	Draining       bool   `json:"draining"`
	Epoch          uint64 `json:"epoch"`
	HighWaterEpoch uint64 `json:"high_water_epoch"`
	IdleFor        string `json:"idle_for,omitempty"`
}

// LivenessHandler serves the runner's /healthz. It stays 200 while the
// runner is booting and while a run is in flight; it fails only when the
// consume loop has stopped cycling, which a restart genuinely fixes.
func (p *HealthProvider) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h, live := p.get()
		if !live {
			writeHealth(w, http.StatusOK, "starting", Health{})
			return
		}
		if !h.Alive() {
			writeHealth(w, http.StatusServiceUnavailable, "stalled", h)
			return
		}
		writeHealth(w, http.StatusOK, "ok", h)
	})
}

// ReadinessHandler serves the runner's /readyz: 503 until the consume
// loop starts and again for the whole drain.
func (p *HealthProvider) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h, live := p.get()
		if !live {
			writeHealth(w, http.StatusServiceUnavailable, "starting", Health{})
			return
		}
		if !h.Ready() {
			status := "starting"
			if h.Superseded {
				status = "superseded"
			} else if h.Draining {
				status = "draining"
			}
			writeHealth(w, http.StatusServiceUnavailable, status, h)
			return
		}
		writeHealth(w, http.StatusOK, "ok", h)
	})
}

func writeHealth(w http.ResponseWriter, code int, status string, h Health) {
	body := healthBody{
		Status:         status,
		Version:        appinfo.Version,
		Commit:         appinfo.Commit,
		Busy:           h.Busy,
		Draining:       h.Draining,
		Epoch:          h.Epoch,
		HighWaterEpoch: h.HighWaterEpoch,
	}
	if h.Started && !h.Busy {
		body.IdleFor = h.SinceLastTick.Round(time.Second).String()
	}
	httpx.WriteJSON(w, code, body)
}
