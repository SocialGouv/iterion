package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// healthResponse is the JSON envelope returned by /healthz and /readyz.
type healthResponse struct {
	Status  string            `json:"status"`            // "ok" or "degraded"
	Mode    string            `json:"mode"`              // "local" or "cloud"
	Version string            `json:"version,omitempty"` // build version
	Commit  string            `json:"commit,omitempty"`  // build commit
	Checks  map[string]string `json:"checks,omitempty"`  // per-dependency status (cloud only)
	// UsageCap echoes the EFFECTIVE usage-cap policy — the DB-backed
	// runtime settings laid over the ITERION_USAGE_CAP_* env defaults
	// (env-only when no settings store is wired). Config, not a secret —
	// a guard nobody can observe is a guard nobody can trust: this is
	// where an operator verifies a runtime cap change actually landed,
	// without reading the DB.
	UsageCap string `json:"usage_cap,omitempty"`
	// UsageCapSource says where the effective percentages came from:
	// "env", "db" or "db+env". Omitted only when the env policy itself
	// is invalid (UsageCap then carries the reason).
	UsageCapSource string `json:"usage_cap_source,omitempty"`
}

// usageCapSummary renders the effective usage-cap policy for the health
// envelope, plus its origin. A malformed env value is reported, not
// hidden — the enforcement paths refuse to start on it, so the probe
// must say why.
func (s *Server) usageCapSummary() (policy, source string) {
	pol, err := usagecap.FromEnv()
	if err != nil {
		return "invalid: " + err.Error(), ""
	}
	if s.usageCapSource == nil {
		return pol.String(), "env"
	}
	eff, origin := s.usageCapSource.EffectiveOrigin(context.Background())
	return eff.String(), origin.String()
}

// defaultReadinessTimeout caps each individual readiness check when the
// caller did not set Config.ReadinessTimeout.
const defaultReadinessTimeout = 1 * time.Second

// readyzProbe is one in-flight dependency ping, shared by every readiness
// request that arrives while it runs. status/failed are written once,
// before done is closed — readers must observe done first.
type readyzProbe struct {
	done   chan struct{}
	status string
	failed bool
}

// probeRef is what one readiness request keeps about a check: the ping it
// is reading (its own or another request's) and the two facts the response
// needs — whether this request launched it, and whether its failure
// evicts the pod.
type probeRef struct {
	probe    *readyzProbe
	shared   bool
	critical bool
}

// readinessWaitGrace is how long past its own deadline a check may take
// to return before the handler stops waiting on it and reports a timeout.
// It exists only to tell a slow-but-context-honouring driver from one
// that ignores its context entirely — the handler must never outlive the
// kubelet's probe timeout, whatever a dependency does.
const readinessWaitGrace = 200 * time.Millisecond

// handleHealthz is the liveness probe. Always returns 200 — its only
// promise is that the HTTP server's mux loop is responsive. Cloud
// deployments use this for the kubelet `livenessProbe`; a 503 here
// would mean restart the pod, which is a much stronger signal than
// "Mongo briefly degraded". It stays 200 through the drain too: the
// readiness probe is what removes a terminating pod from the Service,
// and a liveness failure would instead kill it mid-drain.
//
// Cloud-ready plan §F (T-37).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	capPolicy, capSource := s.usageCapSummary()
	writeHealthJSON(w, http.StatusOK, healthResponse{
		Status:         "ok",
		Mode:           s.deployMode(),
		Version:        appinfo.Version,
		Commit:         appinfo.Commit,
		UsageCap:       capPolicy,
		UsageCapSource: capSource,
	})
}

// handleReadyz is the readiness probe. Returns 503 while the server is
// draining, then 200 when every CRITICAL dependency wired via
// Config.ReadinessChecks responds within ReadinessTimeout. A failing
// non-critical dependency is reported in the checks map under status
// "degraded" but still answers 200 — see ReadinessCheck.Critical for why
// that distinction exists. Local mode has no dependencies and always
// returns 200.
//
// Cloud-ready plan §F (T-37).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	capPolicy, capSource := s.usageCapSummary()
	resp := healthResponse{
		Status:         "ok",
		Mode:           s.deployMode(),
		Version:        appinfo.Version,
		Commit:         appinfo.Commit,
		UsageCap:       capPolicy,
		UsageCapSource: capSource,
	}

	// Lame-duck first, and without touching a single dependency: this pod
	// is on its way out whatever the backends say.
	if s.draining.Load() {
		resp.Status = "draining"
		writeHealthJSON(w, http.StatusServiceUnavailable, resp)
		return
	}

	checks := s.cfg.ReadinessChecks
	if len(checks) == 0 {
		writeHealthJSON(w, http.StatusOK, resp)
		return
	}

	timeout := s.cfg.ReadinessTimeout
	if timeout <= 0 {
		timeout = defaultReadinessTimeout
	}

	// Pinged CONCURRENTLY, so the probe costs max(check) rather than
	// sum(check). Sequentially, four dependencies each allowed a 1s
	// sub-deadline can take 4s — past the kubelet's own probe timeout,
	// which would fail the probe before the handler could answer "one
	// non-critical backend is degraded, stay in the pool". That would
	// hand back the fleet-wide outage the Critical split exists to
	// prevent, on exactly the partial-degradation case it targets.
	// giveUp is the handler's single point of abandonment, closed once it
	// stops waiting. Sharers watch it instead of running timers of their
	// own: two identical deadlines racing made the reported reason flip
	// between runs, and left goroutines ticking after the response.
	var wg sync.WaitGroup
	giveUp := make(chan struct{})
	refs := make(map[string]probeRef, len(checks))
	resp.Checks = make(map[string]string, len(checks))

	for name, check := range checks {
		if check.Ping == nil {
			// A wiring bug, not a dependency state — so it must not evict
			// the pod, but it must be VISIBLE: a declared check that never
			// runs would otherwise read as silent health.
			resp.Checks[name] = "not wired: this check has no Ping"
			continue
		}
		// One ping per check at a time, shared by every request that wants
		// it. Launching a second is what leaks: a driver that ignores its
		// context never returns, the kubelet re-probes every few seconds,
		// and the stranded goroutines eventually OOMKill the pod — a
		// SIGKILL, so no drain at all.
		fresh := &readyzProbe{done: make(chan struct{})}
		stored, busy := s.readyzInflight.LoadOrStore(name, fresh)
		probe := stored.(*readyzProbe)
		refs[name] = probeRef{probe: probe, shared: busy, critical: check.Critical}

		wg.Add(1)
		go func(name string, check ReadinessCheck, probe *readyzProbe, mine bool) {
			defer wg.Done()

			if !mine {
				// Another request is already pinging this dependency: wait
				// for ITS answer rather than inventing one. Reporting a
				// failure here would 503 a perfectly healthy pod whenever
				// two probes overlap — a kubelet tick next to an operator's
				// curl, or an LB health check.
				select {
				case <-probe.done:
				case <-giveUp:
				}
				return
			}

			// Telemetry last, and off the publishing path: errtrack flushes
			// synchronously, and a slow flush must not delay close(done)
			// into the handler's timeout — which would report a panicking
			// check as a timeout.
			var panicked any
			defer func() {
				if panicked != nil {
					errtrack.CapturePanicFields(panicked, map[string]any{"surface": "server.readyz", "check": name})
				}
			}()
			// Publish before releasing: the registry entry goes first (so
			// the next probe launches a fresh ping), then done is closed —
			// which is BOTH the wake-up for sharers and the happens-before
			// edge that makes probe.status safe to read.
			defer func() {
				s.readyzInflight.Delete(name)
				close(probe.done)
			}()
			// This goroutine runs OUTSIDE net/http's per-connection
			// recover, which used to contain a panicking driver when the
			// pings were inline. Unguarded, one would exit the process
			// with no drain at all — cutting every in-flight request and
			// refusing every new connection, the exact failure the
			// lame-duck exists to prevent. Report it and treat the
			// dependency as down.
			defer func() {
				if p := recover(); p != nil {
					panicked = p
					probe.status, probe.failed = fmt.Sprintf("panic: %v", p), true
				}
			}()
			// Detached from r.Context() so a kubelet probe timeout/cancel
			// doesn't propagate into the dependency check and falsely
			// report it as failing — that would briefly remove the pod
			// from the LB on every transient network hiccup.
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			err := check.Ping(ctx)
			cancel()

			probe.status, probe.failed = "ok", false
			if err != nil {
				probe.status, probe.failed = "error: "+err.Error(), true
			}
		}(name, check, probe, !busy)
	}
	// Bound the wait as well. A driver that ignores its context never
	// returns from check.Ping; an unbounded wg.Wait would then hang the
	// handler, which hangs the kubelet probe AND makes
	// http.Server.Shutdown wait on that still-live request for the whole
	// teardown budget. The grace past ReadinessTimeout is what separates a
	// slow-but-honouring driver from a hung one.
	done := make(chan struct{})
	go func() { defer close(done); wg.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout + readinessWaitGrace):
	}
	close(giveUp) // release the sharers; they have nothing left to report

	// `close(probe.done)` is the single oracle: it is both the
	// happens-before edge for probe.status and the answer to "did this
	// check finish?". Reading it here, rather than having each goroutine
	// write into a shared map, removes the window where a ping that
	// returned microseconds before the deadline still got reported as a
	// timeout — a 503 on a pod whose dependency had just answered.
	var degraded, evict bool
	for name, ref := range refs {
		status, failed := "", true
		select {
		case <-ref.probe.done:
			status, failed = ref.probe.status, ref.probe.failed
		default:
			// A check this request SHARED is one whose ping predates it —
			// worth saying, because it means the driver has been
			// unresponsive for longer than a single probe, not merely slow
			// this time.
			status = "timeout: no answer within " + (timeout + readinessWaitGrace).String()
			if ref.shared {
				status = "stalled: an earlier probe's ping has not returned"
			}
		}
		resp.Checks[name] = status
		if failed {
			degraded = true
			evict = evict || ref.critical
		}
	}

	if degraded {
		resp.Status = "degraded"
	}
	if evict {
		writeHealthJSON(w, http.StatusServiceUnavailable, resp)
		return
	}
	writeHealthJSON(w, http.StatusOK, resp)
}

// deployMode reports the persistence backend in use. Reads Config.Mode
// when set (cloud bootstrap path), otherwise falls back to the
// store-directory heuristic so existing local-only callers keep
// working without a Config update.
func (s *Server) deployMode() string {
	if s.cfg.Mode != "" {
		return s.cfg.Mode
	}
	if s.runs == nil {
		return "local"
	}
	if s.runs.StoreDir() == "" {
		return "cloud"
	}
	return "local"
}

// writeHealthJSON is a one-liner JSON response helper for the health
// endpoints. Not exported — the rest of the server composes responses
// inline today.
func writeHealthJSON(w http.ResponseWriter, status int, payload any) {
	httpx.WriteJSON(w, status, payload)
}
