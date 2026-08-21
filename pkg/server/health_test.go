package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// /healthz must always return 200 even when the run console is
// disabled — the kubelet liveness probe relies on this contract.
func TestHealthzAlwaysOK(t *testing.T) {
	t.Parallel()

	srv := New(Config{}, iterlog.New(iterlog.LevelError, nil))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.handler = srv.mux
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q, want ok", payload.Status)
	}
	if payload.Mode != "local" {
		t.Errorf("mode = %q, want local for filesystem store", payload.Mode)
	}
}

// The health envelope echoes the usage-cap policy so an operator can
// verify the cap actually reached the deployment — the enforcement is
// env-only and was otherwise observable nowhere.
func TestHealthzEchoesUsageCap(t *testing.T) {
	probe := func() healthResponse {
		srv := New(Config{}, iterlog.New(iterlog.LevelError, nil))
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		srv.handler = srv.mux
		srv.handler.ServeHTTP(rec, req)
		var payload healthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return payload
	}

	t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "85")
	if got := probe().UsageCap; !strings.Contains(got, "week=85%/hard") {
		t.Errorf("usage_cap = %q, want it to name week=85%%/hard", got)
	}

	t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "")
	if got := probe().UsageCap; got != "usage caps off" {
		t.Errorf("usage_cap = %q, want %q when unset", got, "usage caps off")
	}

	// A malformed value is reported, never hidden.
	t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "eighty-five")
	if got := probe().UsageCap; !strings.HasPrefix(got, "invalid: ") {
		t.Errorf("usage_cap = %q, want an invalid: prefix on a malformed value", got)
	}
}

// /readyz returns 200 in local mode (no dependencies to ping). Cloud
// pings come via T-26 once Mongo/NATS/S3 are wired into the server's
// dependency graph.
func TestReadyzLocalReturnsOK(t *testing.T) {
	t.Parallel()

	srv := New(Config{}, iterlog.New(iterlog.LevelError, nil))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.handler = srv.mux
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// probe drives one health endpoint against a server and returns the
// status code + decoded envelope.
func probeHealth(t *testing.T, srv *Server, path string) (int, healthResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.handler = srv.mux
	srv.handler.ServeHTTP(rec, req)
	var payload healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %s body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, payload
}

// A failing NON-critical dependency must not remove the pod from the
// Service: every replica pings the same backends, so a 503 here turns a
// partial degradation into a fleet-wide outage. The failure is still
// reported — visible, just not fatal.
//
// Mutation coverage: make every check gate readiness → the first case's
// 200 assertion fires.
func TestReadyzCriticalVsDegraded(t *testing.T) {
	t.Parallel()

	down := func(ctx context.Context) error { return errors.New("connection refused") }
	up := func(ctx context.Context) error { return nil }

	cases := []struct {
		name       string
		checks     map[string]ReadinessCheck
		wantCode   int
		wantStatus string
	}{
		{
			name:       "all up",
			checks:     map[string]ReadinessCheck{"mongo": {Ping: up, Critical: true}, "s3": {Ping: up}},
			wantCode:   http.StatusOK,
			wantStatus: "ok",
		},
		{
			name:       "non-critical down stays in the pool",
			checks:     map[string]ReadinessCheck{"mongo": {Ping: up, Critical: true}, "s3": {Ping: down}},
			wantCode:   http.StatusOK,
			wantStatus: "degraded",
		},
		{
			name:       "critical down leaves the pool",
			checks:     map[string]ReadinessCheck{"mongo": {Ping: down, Critical: true}, "s3": {Ping: up}},
			wantCode:   http.StatusServiceUnavailable,
			wantStatus: "degraded",
		},
		{
			// A wiring bug must not evict the pod, but it must not read as
			// health either — a check that silently never runs is worse
			// than one that fails.
			name:       "unwired check is reported, not silent",
			checks:     map[string]ReadinessCheck{"mongo": {Ping: up, Critical: true}, "ghost": {Critical: true}},
			wantCode:   http.StatusOK,
			wantStatus: "ok",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := New(Config{ReadinessChecks: c.checks}, iterlog.New(iterlog.LevelError, nil))
			code, payload := probeHealth(t, srv, "/readyz")
			if code != c.wantCode {
				t.Errorf("status = %d, want %d (checks: %v)", code, c.wantCode, payload.Checks)
			}
			if payload.Status != c.wantStatus {
				t.Errorf("status field = %q, want %q", payload.Status, c.wantStatus)
			}
			if len(payload.Checks) != len(c.checks) {
				t.Errorf("checks = %v, want one entry per dependency", payload.Checks)
			}
			// A declared-but-unwired check must be visible, not silently
			// absent — an omitted entry reads as health.
			if c.name == "unwired check is reported, not silent" {
				if got := payload.Checks["ghost"]; !strings.Contains(got, "not wired") {
					t.Errorf("ghost check = %q, want it named as unwired", got)
				}
			}
			// Liveness never follows a dependency: a 503 here would make
			// the kubelet restart a pod whose backend is merely blipping.
			if liveCode, _ := probeHealth(t, srv, "/healthz"); liveCode != http.StatusOK {
				t.Errorf("/healthz = %d, want 200 regardless of dependencies", liveCode)
			}
		})
	}
}

// A panicking dependency ping must not take the process down. The pings
// run in their own goroutines, OUTSIDE net/http's per-connection recover
// — so an unguarded panic there is an un-drained process exit: every
// in-flight request cut, every connection refused, which is the exact
// failure this whole change exists to prevent. The dependency is instead
// reported as down, and its Critical flag decides the status code.
//
// Mutation coverage: remove the recover in handleReadyz's goroutine →
// this test crashes the test binary instead of failing.
func TestReadyzSurvivesAPanickingCheck(t *testing.T) {
	t.Parallel()

	srv := New(Config{ReadinessChecks: map[string]ReadinessCheck{
		"mongo": {Ping: func(ctx context.Context) error { return nil }, Critical: true},
		"s3":    {Ping: func(ctx context.Context) error { panic("malformed response") }},
	}}, iterlog.New(iterlog.LevelError, nil))

	code, payload := probeHealth(t, srv, "/readyz")

	// s3 is non-critical: the pod stays in the pool, and says why.
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 (a non-critical panic must not evict the pod); checks: %v", code, payload.Checks)
	}
	if payload.Status != "degraded" {
		t.Errorf("status field = %q, want degraded", payload.Status)
	}
	if got := payload.Checks["s3"]; !strings.Contains(got, "panic") {
		t.Errorf("s3 check = %q, want it to name the panic", got)
	}
	if got := payload.Checks["mongo"]; got != "ok" {
		t.Errorf("mongo check = %q, want ok — one panicking check must not poison the others", got)
	}

	// A panicking CRITICAL check evicts the pod, like any other failure.
	crit := New(Config{ReadinessChecks: map[string]ReadinessCheck{
		"mongo": {Ping: func(ctx context.Context) error { panic("driver bug") }, Critical: true},
	}}, iterlog.New(iterlog.LevelError, nil))
	if code, payload := probeHealth(t, crit, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("critical panic → %d, want 503; checks: %v", code, payload.Checks)
	}
}

// A dependency ping that ignores its context must not hang the handler.
// The probe would then outlive the kubelet's own timeout, and — worse —
// http.Server.Shutdown would wait on that still-live request for the
// whole teardown budget, spending the graceful shutdown on a wedged
// driver instead of on real traffic.
//
// Run under -race: on the timeout path the check's goroutine is still
// running, so the handler must read `degraded`/`criticalDown` under the
// same mutex the goroutine writes them with.
//
// Mutation coverage: drop the bounded select in handleReadyz → this test
// times out; read the flags outside the mutex → `go test -race` reports
// a race between health.go's write and its read.
func TestReadyzDoesNotHangOnAContextIgnoringCheck(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv := New(Config{
		ReadinessTimeout: 50 * time.Millisecond,
		ReadinessChecks: map[string]ReadinessCheck{
			"wedged": {Ping: func(ctx context.Context) error { <-release; return nil }, Critical: true},
			"fine":   {Ping: func(ctx context.Context) error { return nil }},
		},
	}, iterlog.New(iterlog.LevelError, nil))

	start := time.Now()
	code, payload := probeHealth(t, srv, "/readyz")
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("handler took %s on a 50ms deadline — it is waiting on a hung check", elapsed)
	}
	// The wedged check is critical, so the pod leaves the pool — and says
	// which dependency did not answer rather than reporting it as healthy.
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; checks: %v", code, payload.Checks)
	}
	if got := payload.Checks["wedged"]; !strings.Contains(got, "timeout") {
		t.Errorf("wedged check = %q, want a timeout report", got)
	}
	if got := payload.Checks["fine"]; got != "ok" {
		t.Errorf("fine check = %q, want ok — one hung check must not hide the others", got)
	}
}

// Bounding the handler's WAIT is not enough: the goroutines it abandoned
// are still there. A driver that ignores its context never returns, the
// kubelet re-probes every few seconds, and each probe would strand
// another pair — measured at ~5.7 KB retained per probe, ~100 MB/day,
// ending in an OOMKill. That is a SIGKILL: no drain, no lame-duck, the
// exact outcome this whole change exists to avoid. So a check still
// running from an earlier probe is not re-launched.
//
// Mutation coverage: drop the readyzInflight guard in handleReadyz → the
// leak grows with the probe count and the bound below fails (measured
// 100 leaked goroutines over 50 probes before the fix, 2 after).
func TestReadyzDoesNotRelaunchAWedgedCheck(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var launches atomic.Int32
	srv := New(Config{
		ReadinessTimeout: 20 * time.Millisecond,
		ReadinessChecks: map[string]ReadinessCheck{
			"wedged": {Critical: true, Ping: func(ctx context.Context) error {
				launches.Add(1)
				<-release
				return nil
			}},
		},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	const probes = 5
	var lastCode int
	var last healthResponse
	for i := 0; i < probes; i++ {
		lastCode, last = probeHealth(t, srv, "/readyz")
	}

	// ONE launch, however many probes. Counting invocations rather than
	// goroutines keeps the assertion deterministic (other tests in this
	// package run in parallel) and tests the mechanism directly.
	if got := launches.Load(); got != 1 {
		t.Errorf("the wedged check was launched %d times over %d probes — every extra launch strands a goroutine for good", got, probes)
	}
	// And it stays honest: a stalled critical dependency evicts the pod.
	if lastCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — a stalled critical dependency must evict the pod", lastCode)
	}
	if got := last.Checks["wedged"]; !strings.Contains(got, "stalled") {
		t.Errorf("wedged check = %q, want it to report the stall", got)
	}

	// Recovery: once the driver finally answers, the pod must return to
	// the pool AND the registry entry must be released so later probes
	// ping again. A one-way latch would keep a healthy pod out for good.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if code, payload := probeHealth(t, srv, "/readyz"); code == http.StatusOK && payload.Checks["wedged"] == "ok" {
			recovered = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("the check never recovered after its driver answered — the pod stays out of the Service for good")
	}
	// That first 200 may legitimately be the freed ping's own answer,
	// shared. What must also hold is that the NEXT probe runs a new one.
	time.Sleep(50 * time.Millisecond)
	probeHealth(t, srv, "/readyz")
	if got := launches.Load(); got < 2 {
		t.Errorf("still %d launch(es) after recovery — the registry is a one-way latch", got)
	}
}

// A check that already answered must never be reported as a timeout,
// even when ANOTHER check is what forced the handler to wait out its
// whole deadline. The report is derived from one atomic observation of
// each probe's done channel — a single oracle. The earlier shape had two
// (a mutex-guarded map written by the goroutines, read by the handler)
// and they could disagree: a ping that returned microseconds before the
// deadline could still be published as "timeout", evicting a pod whose
// dependency had just answered.
//
// Mutation coverage: report from anything other than probe.done (a shared
// map the goroutines write under a mutex) and the fast check starts
// reading "timeout" whenever the handler wins the lock race.
func TestReadyzNeverMisreportsAFinishedCheck(t *testing.T) {
	t.Parallel()

	wedge := make(chan struct{})
	t.Cleanup(func() { close(wedge) })

	srv := New(Config{
		ReadinessTimeout: 30 * time.Millisecond,
		ReadinessChecks: map[string]ReadinessCheck{
			"fast":   {Ping: func(ctx context.Context) error { return nil }},
			"wedged": {Ping: func(ctx context.Context) error { <-wedge; return nil }},
		},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	for i := 0; i < 20; i++ {
		code, payload := probeHealth(t, srv, "/readyz")
		if got := payload.Checks["fast"]; got != "ok" {
			t.Fatalf("iteration %d: fast check = %q, want ok — it answered instantly", i, got)
		}
		if code != http.StatusOK {
			t.Fatalf("iteration %d: status = %d, want 200 — neither check is critical", i, code)
		}
	}
}

// Two probes overlapping on a HEALTHY dependency must both get its real
// answer. Reporting the second as stalled would 503 a perfectly good pod
// whenever a kubelet tick meets an operator's curl or an LB health check
// — and in a mechanism that EVICTS, a false positive costs as much as a
// missed failure.
//
// Mutation coverage: make the losing probe report "stalled" instead of
// waiting on the shared result → 7 of these 8 come back 503.
func TestReadyzConcurrentProbesShareOneAnswer(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	var launches atomic.Int32
	srv := New(Config{
		ReadinessTimeout: time.Second,
		ReadinessChecks: map[string]ReadinessCheck{
			"mongo": {Critical: true, Ping: func(ctx context.Context) error {
				launches.Add(1)
				<-gate
				return nil
			}},
		},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	const probes = 8
	var wg sync.WaitGroup
	codes := make([]int, probes)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			srv.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			codes[i] = rec.Code
		}(i)
	}
	time.Sleep(80 * time.Millisecond) // let them all pile onto the same check
	close(gate)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("probe %d = %d, want 200 — the dependency is healthy; codes: %v", i, c, codes)
			break
		}
	}
	if got := launches.Load(); got != 1 {
		t.Errorf("the check ran %d times for %d overlapping probes, want 1 shared run", got, probes)
	}
}

// The lame-duck window: once Shutdown starts, /readyz must report 503
// WITHOUT pinging anything (this pod is leaving whatever the backends
// say) while /healthz stays 200 (a liveness failure would kill the pod
// mid-drain). This is what lets the endpoints controller pull the pod
// before its listener closes.
//
// Mutation coverage: drop the draining check in handleReadyz → the 503
// assertion fires; make handleHealthz honour draining → the 200 fires.
func TestReadyzDrainingIs503AndSkipsChecks(t *testing.T) {
	t.Parallel()

	pinged := 0
	srv := New(Config{ReadinessChecks: map[string]ReadinessCheck{
		"mongo": {Ping: func(ctx context.Context) error { pinged++; return nil }, Critical: true},
	}}, iterlog.New(iterlog.LevelError, nil))

	if code, _ := probeHealth(t, srv, "/readyz"); code != http.StatusOK {
		t.Fatalf("pre-drain /readyz = %d, want 200", code)
	}

	srv.beginDrain(context.Background())
	before := pinged

	code, payload := probeHealth(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("draining /readyz = %d, want 503", code)
	}
	if payload.Status != "draining" {
		t.Errorf("draining /readyz status = %q, want draining", payload.Status)
	}
	if pinged != before {
		t.Errorf("dependencies pinged %d time(s) while draining, want 0", pinged-before)
	}
	if liveCode, livePayload := probeHealth(t, srv, "/healthz"); liveCode != http.StatusOK || livePayload.Status != "ok" {
		t.Errorf("draining /healthz = %d %q, want 200 ok (a liveness failure kills the pod mid-drain)", liveCode, livePayload.Status)
	}
}

// beginDrain must hold the listener open for ShutdownDelay — that pause
// IS the fix — but never past the caller's deadline, so a tight budget
// (or a second Ctrl-C) still gets through.
func TestBeginDrainWaitsButRespectsContext(t *testing.T) {
	t.Parallel()

	srv := New(Config{ShutdownDelay: 60 * time.Second}, iterlog.New(iterlog.LevelError, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	srv.beginDrain(ctx)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("beginDrain waited %s past a 50ms deadline", elapsed)
	}
	if !srv.draining.Load() {
		t.Error("draining flag not set")
	}

	fast := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	start = time.Now()
	fast.beginDrain(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a zero ShutdownDelay waited %s, want no wait", elapsed)
	}
}

// The run drain and the HTTP shutdown must not share one budget: a drain
// that consumes everything would leave http.Server.Shutdown an expired
// context, cutting the very requests we are trying to let finish.
func TestDrainBudgetLeavesRoomForTheHTTPShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sub, subCancel := drainBudget(ctx)
	defer subCancel()

	deadline, ok := sub.Deadline()
	if !ok {
		t.Fatal("drain budget has no deadline")
	}
	if got := time.Until(deadline); got > 25*time.Second {
		t.Errorf("drain budget = %s of 30s, want a reserve for the HTTP shutdown", got)
	}

	// An undeadlined parent passes through: nothing to divide.
	plain, plainCancel := drainBudget(context.Background())
	defer plainCancel()
	if _, ok := plain.Deadline(); ok {
		t.Error("drain budget invented a deadline the caller never set")
	}
}

// Readiness pings every dependency CONCURRENTLY. Sequentially, four
// backends each allowed a 1s sub-deadline take 4s — past the kubelet's
// own probe timeout, which fails the probe before the handler can report
// "one non-critical backend is degraded, keep me in the pool". That is
// the fleet-wide outage the Critical split exists to prevent, on exactly
// the case it targets.
//
// Mutation coverage: make the loop sequential again → this exceeds the
// bound and fails.
func TestReadyzPingsDependenciesConcurrently(t *testing.T) {
	t.Parallel()

	const perCheck = 300 * time.Millisecond
	slow := func(ctx context.Context) error {
		select {
		case <-time.After(perCheck):
		case <-ctx.Done():
		}
		return nil
	}
	srv := New(Config{
		ReadinessTimeout: time.Second,
		ReadinessChecks: map[string]ReadinessCheck{
			"mongo":  {Ping: slow, Critical: true},
			"nats":   {Ping: slow},
			"s3":     {Ping: slow},
			"valkey": {Ping: slow},
		},
	}, iterlog.New(iterlog.LevelError, nil))

	start := time.Now()
	code, payload := probeHealth(t, srv, "/readyz")
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; checks: %v", code, payload.Checks)
	}
	if len(payload.Checks) != 4 {
		t.Fatalf("checks = %v, want all four reported", payload.Checks)
	}
	// Concurrent ⇒ ~perCheck. Sequential ⇒ ~4×. Bound at 2× so a slow
	// CI box does not flake while a sequential regression still trips.
	if elapsed > 2*perCheck {
		t.Errorf("four %s checks took %s — they are running sequentially", perCheck, elapsed)
	}
}
