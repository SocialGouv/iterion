package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeCapacityLister answers the alive listing from fixed runs, or an error.
type fakeCapacityLister struct {
	runs []store.Run
	err  error
}

func (f *fakeCapacityLister) ListAliveRunsForCapacity(context.Context) ([]store.Run, error) {
	return f.runs, f.err
}

const (
	gatewayAliveSeriesA     = `iterion_gateway_alive_runs{host="a.example"}`
	gatewayAliveUnavailable = `iterion_gateway_alive_runs_unavailable`
)

// aliveOnGateway is a parent on gw:a.example and its child resumed on its own
// after a human gate, stamped with the same subject: two runs, one tree.
func aliveOnGateway() []store.Run {
	subject := store.GatewayCapacitySubject("a.example")
	return []store.Run{
		{ID: "parent", Status: store.RunStatusRunning, CapacitySubjects: []string{subject}},
		{ID: "child", Status: store.RunStatusQueued, ParentRunID: "parent", ParentNodeID: "review", CapacitySubjects: []string{subject}},
	}
}

// Before any pass the gauge says it has no reading; after one, a host's value
// is its alive trees (a tree is one place), labelled by the bare host.
func TestGatewayAliveRunsGauge_OnePassPublishesTreesPerHost(t *testing.T) {
	reg := cloudmetrics.New()
	lister := &fakeCapacityLister{runs: aliveOnGateway()}
	o := newGatewayAliveRunsObserver(lister, reg, nil)
	if got := scrapedGauges(t, reg); got[gatewayAliveUnavailable] != 1 {
		t.Fatalf("before any pass: %s = %v, want 1 — no reading must not read as no alive run", gatewayAliveUnavailable, got[gatewayAliveUnavailable])
	}
	o.observe(context.Background())
	got := scrapedGauges(t, reg)
	if v, ok := got[gatewayAliveSeriesA]; !ok || v != 1 {
		t.Fatalf("after one pass: %s = %v (present %v), want 1 — the parent and its child are one tree; series: %v", gatewayAliveSeriesA, v, ok, got)
	}
	assertPublishedZero(t, got, gatewayAliveUnavailable, "after a successful pass")
}

// A failed read holds the per-host values and raises the unavailable gauge.
// One outage costs one warning; a recovery re-arms it, so the next outage is
// its own event and speaks.
func TestGatewayAliveRunsGauge_AFailedReadHoldsTheLastValues(t *testing.T) {
	reg := cloudmetrics.New()
	lister := &fakeCapacityLister{runs: aliveOnGateway()}
	var warned []string
	o := newGatewayAliveRunsObserver(lister, reg, func(format string, _ ...any) { warned = append(warned, format) })
	o.observe(context.Background())
	lister.runs, lister.err = nil, errors.New("runs collection unavailable")
	o.observe(context.Background())
	o.observe(context.Background())
	got := scrapedGauges(t, reg)
	if v, ok := got[gatewayAliveSeriesA]; !ok || v != 1 {
		t.Fatalf("after a failed read: %s = %v (present %v), want the last value 1 held", gatewayAliveSeriesA, v, ok)
	}
	if got[gatewayAliveUnavailable] != 1 {
		t.Fatalf("after a failed read: %s = %v, want 1", gatewayAliveUnavailable, got[gatewayAliveUnavailable])
	}
	if len(warned) != 1 {
		t.Fatalf("warned %d time(s) over two failed passes, want once, on the rising edge", len(warned))
	}

	lister.runs, lister.err = aliveOnGateway(), nil
	o.observe(context.Background())
	assertPublishedZero(t, scrapedGauges(t, reg), gatewayAliveUnavailable, "after the read recovered")
	lister.runs, lister.err = nil, errors.New("runs collection unavailable again")
	o.observe(context.Background())
	if len(warned) != 2 {
		t.Fatalf("warned %d time(s) over two outages separated by a recovery, want twice — a flag that only ever rises silences every later outage", len(warned))
	}
}

// A host whose last tree ended has no series on the next pass.
func TestGatewayAliveRunsGauge_AHostWithNoAliveRunLosesItsSeries(t *testing.T) {
	reg := cloudmetrics.New()
	lister := &fakeCapacityLister{runs: aliveOnGateway()}
	o := newGatewayAliveRunsObserver(lister, reg, nil)
	o.observe(context.Background())
	if _, ok := scrapedGauges(t, reg)[gatewayAliveSeriesA]; !ok {
		t.Fatalf("fixture: %s not published after the first pass", gatewayAliveSeriesA)
	}
	lister.runs = nil
	o.observe(context.Background())
	got := scrapedGauges(t, reg)
	assertNoSeries(t, got, gatewayAliveSeriesA, "after the host's last run ended")
	assertPublishedZero(t, got, gatewayAliveUnavailable, "after a successful pass")
}

// hangingCapacityLister answers its first listing, then hangs every later one
// until its context ends — a read stuck on an established connection. hung
// closes when the first hanging call starts: the passes are sequential, so
// the first pass has published by then.
type hangingCapacityLister struct {
	runs  []store.Run
	calls atomic.Int32
	hung  chan struct{}
}

func newHangingCapacityLister(runs []store.Run) *hangingCapacityLister {
	return &hangingCapacityLister{runs: runs, hung: make(chan struct{})}
}

func (h *hangingCapacityLister) ListAliveRunsForCapacity(ctx context.Context) ([]store.Run, error) {
	switch h.calls.Add(1) {
	case 1:
		return h.runs, nil
	case 2:
		close(h.hung)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// waitClosed waits for ch, or fails the test after a generous bound.
func waitClosed(t *testing.T, ch <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out: %s", why)
	}
}

// waitForGauge polls the registry until series reads want, or fails the test
// after a bound far above the pass deadline under test.
func waitForGauge(t *testing.T, reg *cloudmetrics.Registry, series string, want float64, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v, ok := scrapedGauges(t, reg)[series]; ok && v == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never read %v — %s", series, want, why)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A pass whose listing hangs fails at its deadline and raises the unavailable
// gauge: without a bound the values freeze while the gauge still reads
// available, which is a clean bill of health nobody issued.
func TestRunGatewayAliveRunsObserver_AHungReadFailsItsPass(t *testing.T) {
	reg := cloudmetrics.New()
	lister := newHangingCapacityLister(aliveOnGateway())
	o := newGatewayAliveRunsObserver(lister, reg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.run(ctx, 40*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitClosed(t, lister.hung, "the second pass never listed")
	waitForGauge(t, reg, gatewayAliveUnavailable, 1, "a listing that hangs must fail its pass at the deadline and raise the flag")
	if v := scrapedGauges(t, reg)[gatewayAliveSeriesA]; v != 1 {
		t.Fatalf("%s = %v after a hung pass, want the last value 1 held", gatewayAliveSeriesA, v)
	}
}

// A restart re-phases the ticker onto the pod's start time: without a pass
// BEFORE the first tick a whole interval goes by with no per-host series at
// all. The interval is an hour and the context already ended: only the boot
// pass can have published anything.
func TestRunGatewayAliveRunsObserver_ABootPassPrecedesTheFirstTick(t *testing.T) {
	reg := cloudmetrics.New()
	o := newGatewayAliveRunsObserver(&fakeCapacityLister{runs: aliveOnGateway()}, reg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.run(ctx, time.Hour)
	got := scrapedGauges(t, reg)
	if v, ok := got[gatewayAliveSeriesA]; !ok || v != 1 {
		t.Fatalf("%s = %v (present %v), want 1 published by a boot pass before the first tick", gatewayAliveSeriesA, v, ok)
	}
	assertPublishedZero(t, got, gatewayAliveUnavailable, "after the boot pass")
}

// gatewayObserverServer is a server with a metrics registry over rs, whose
// shutdown has already fired: goUntilShutdown then refuses the loop, so the
// arming is the only thing that can have touched the gauges.
func gatewayObserverServer(t *testing.T, rs store.RunStore) (*Server, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	s := New(Config{Store: rs, Metrics: cloudmetrics.New()}, iterlog.New(iterlog.LevelInfo, logs))
	close(s.shutdown)
	return s, logs
}

// A loop that never ran must not read as a quiet fleet: the arming raises the
// unavailable gauge before the loop is spawned.
func TestStartGatewayAliveRunsObserver_ArmsUnavailableBeforeTheLoop(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, _ := gatewayObserverServer(t, rs)
	if _, present := scrapedGauges(t, s.cfg.Metrics)[gatewayAliveUnavailable]; present {
		t.Fatalf("premise: a registry nobody sampled publishes no %s", gatewayAliveUnavailable)
	}
	s.startGatewayAliveRunsObserver()
	if got := scrapedGauges(t, s.cfg.Metrics)[gatewayAliveUnavailable]; got != 1 {
		t.Fatalf("%s = %v after the arming, want 1 — an observer that never ran must not publish a clean reading", gatewayAliveUnavailable, got)
	}
}

// A run store that does not host the listing leaves the gauge unavailable,
// said once at boot — never the absence a quiet fleet publishes.
func TestStartGatewayAliveRunsObserver_AStoreWithoutTheListingIsUnavailable(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, logs := gatewayObserverServer(t, opaqueRunStore{rs})
	s.startGatewayAliveRunsObserver()
	if got := scrapedGauges(t, s.cfg.Metrics)[gatewayAliveUnavailable]; got != 1 {
		t.Fatalf("%s = %v over a store without the listing, want 1", gatewayAliveUnavailable, got)
	}
	if !strings.Contains(logs.String(), "gateway alive-runs observer NOT started") {
		t.Fatalf("the missing listing was not said at boot; logs:\n%s", logs.String())
	}
}

// The arming STARTS the loop. With the shutdown still open, the boot pass
// runs at once over the store's own listing: the host of a run stamped on a
// gateway gets its series, and the unavailable gauge comes down. An arming
// that raised the flag and never spawned the loop would hold it at 1 forever
// while every unit test of the loop stays green.
func TestStartGatewayAliveRunsObserver_StartsTheSamplingLoop(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := rs.CreateRun(ctx, "on-gateway", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = store.RunStatusRunning
	store.RunCredStamp{Fingerprints: []string{"fp-a"}, CapacitySubjects: []string{store.GatewayCapacitySubject("a.example")}, CapacityStamped: true}.Apply(r)
	if err := rs.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Store: rs, Metrics: cloudmetrics.New()}, iterlog.New(iterlog.LevelError, io.Discard))
	t.Cleanup(func() {
		close(s.shutdown)
		join, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.joinBackgroundWorkers(join)
	})
	s.startGatewayAliveRunsObserver()
	waitForGauge(t, s.cfg.Metrics, gatewayAliveSeriesA, 1, "the armed loop's boot pass must publish the host of the stamped run")
	waitForGauge(t, s.cfg.Metrics, gatewayAliveUnavailable, 0, "the armed loop's boot pass must lower the unavailable gauge")
}

// A sampler defined and never STARTED publishes nothing, which reads like a
// quiet fleet. Asserted on the parsed source because no test boots
// ListenAndServe past its recovery-passive return (assertArmedAtBoot): the
// call must be a statement of its body, so one commented out, in a dead
// branch or in another function does not count.
func TestServerLifecycle_ArmsTheGatewayAliveRunsObserver(t *testing.T) {
	assertArmedAtBoot(t, "startGatewayAliveRunsObserver")
}
