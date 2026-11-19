package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
)

// outcomeHookStore is the gate store as the cloud publisher holds it, with a
// one-shot hook run on ENTRY of UpdateRunOutcome, before it delegates: a
// successor recreated after the publisher's load, inside its flip. Every
// call is forwarded with the caller's context.
type outcomeHookStore struct {
	*gateStore
	mu      sync.Mutex
	before  func() error
	hookErr error
}

func (s *outcomeHookStore) UpdateRunOutcome(ctx context.Context, id string, status store.RunStatus, runErr string, meta store.RunOutcomeMeta, expectedFrom []store.RunStatus) (bool, error) {
	s.mu.Lock()
	h := s.before
	s.before = nil
	s.mu.Unlock()
	if h != nil {
		if err := h(); err != nil {
			s.mu.Lock()
			s.hookErr = errors.Join(s.hookErr, err)
			s.mu.Unlock()
		}
	}
	return s.gateStore.UpdateRunOutcome(ctx, id, status, runErr, meta, expectedFrom)
}

func (s *outcomeHookStore) armFlip(h func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before = h
}

func (s *outcomeHookStore) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.hookErr, s.gateStore.failure())
}

// cancelFixture is the production chain over the gate store with one run
// born A. A cloud fixture serves a publisher-backed run service: the real
// cloudpublisher.Publisher over the gate store, its NATS cancel subject
// recorded.
type cancelFixture struct {
	*gateFixture
	flips   *outcomeHookStore
	status  store.RunStatus
	birth   string
	mu      sync.Mutex
	cancels []string
}

func newCancelFixture(t *testing.T, cloud bool, status store.RunStatus) *cancelFixture {
	t.Helper()
	fx := &cancelFixture{gateFixture: newGateFixture(t, true), status: status}
	if cloud {
		fx.flips = &outcomeHookStore{gateStore: fx.store}
		pub := cloudpublisher.NewForTesting(cloudpublisher.Config{Store: fx.flips}, func(context.Context, *queue.RunMessage) error { return nil })
		pub.SetCancelForTesting(func(id string, _ store.LeaseIdentity) error {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			fx.cancels = append(fx.cancels, id)
			return nil
		})
		fx.srv.runs.Stop(t.Context())
		svc := newTestRunviewService(t, fx.srv.cfg.StoreDir, runview.WithRecoveryPassive(), runview.WithStore(fx.store), runview.WithLaunchPublisher(pub))
		streams := &testOwnedRunStreams{Source: svc.StreamSource()}
		runview.WithStreamSource(streams)(svc)
		fx.srv.runs = svc
		t.Cleanup(func() {
			if err := streams.Close(); err != nil {
				t.Errorf("join run streams: %v", err)
			}
		})
	}
	mustWS(t, seedWSOriginRun(t.Context(), fx.native, actionRunID, "A", store.LegacyValueFormat, status, 0))
	fx.birth = birthOf(t, fx.native, actionRunID)
	return fx
}

// replacement replaces A by B in the fixture's status and records B's birth.
type cancelReplacement struct {
	mu    sync.Mutex
	birth string
}

func (fx *cancelFixture) replace(rp *cancelReplacement) func() error {
	return func() error {
		ctx := context.Background()
		if err := replaceWSOriginRun(ctx, fx.native, actionRunID, "B", store.LegacyValueFormat, fx.status, 0); err != nil {
			return err
		}
		b, err := fx.native.LoadRun(ctx, actionRunID)
		if err != nil {
			return err
		}
		rp.mu.Lock()
		rp.birth = b.BirthID
		rp.mu.Unlock()
		return nil
	}
}

func (fx *cancelFixture) natsCancels() []string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]string(nil), fx.cancels...)
}

func (fx *cancelFixture) failure() error {
	if fx.flips != nil {
		return fx.flips.failure()
	}
	return fx.store.failure()
}

// assertSuccessorKept: the replacement ran, the successor keeps the status
// it was seeded with, and no NATS cancel was signalled.
func (fx *cancelFixture) assertSuccessorKept(t *testing.T, rp *cancelReplacement) {
	t.Helper()
	mustWS(t, fx.failure())
	rp.mu.Lock()
	birth := rp.birth
	rp.mu.Unlock()
	if birth == "" || birth == fx.birth {
		t.Fatalf("the replacement did not run: successor birth %q, original %q", birth, fx.birth)
	}
	b, err := fx.native.LoadRun(context.Background(), actionRunID)
	mustWS(t, err)
	if b.BirthID != birth || b.Status != fx.status {
		t.Fatalf("the successor is %s at birth %q, want %s at %q as seeded", b.Status, b.BirthID, fx.status, birth)
	}
	if got := fx.natsCancels(); len(got) != 0 {
		t.Fatalf("NATS cancels signalled for a refused cancel: %v", got)
	}
}

// assertSuccessorCancelled: the legacy outcome — the run behind the ID now,
// the successor, was cancelled.
func (fx *cancelFixture) assertSuccessorCancelled(t *testing.T, rp *cancelReplacement) {
	t.Helper()
	mustWS(t, fx.failure())
	rp.mu.Lock()
	birth := rp.birth
	rp.mu.Unlock()
	b, err := fx.native.LoadRun(context.Background(), actionRunID)
	mustWS(t, err)
	if birth == "" || birth == fx.birth || b.BirthID != birth || b.Status != store.RunStatusCancelled {
		t.Fatalf("legacy cancel: the run is %s at birth %q (successor %q, original %q), want the successor cancelled", b.Status, b.BirthID, birth, fx.birth)
	}
}

var cancelRow = actionRow{method: http.MethodPost, path: "/cancel"}

// W13 (HTTP, cloud): the cancel flip is a store write bound to the admitted
// run. (i) A replaced by B right after the gate: the handler's bound load,
// the pre-check, refuses. (ii) A replaced inside the publisher's flip, after
// its load: the bound flip refuses. Both: 409 run_origin_lost, B keeps its
// status, no NATS cancel for the ID. Control: at the current birth the flip
// lands and the subject is signalled once.
func TestCancelBindsTheFlip(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(fx *cancelFixture, h func() error)
	}{
		{"i-replaced-after-the-gate", func(fx *cancelFixture, h func() error) { fx.store.afterLoadN(1, h) }},
		{"ii-replaced-inside-the-flip", func(fx *cancelFixture, h func() error) { fx.flips.armFlip(h) }},
	} {
		t.Run("cloud-http-"+tc.name, func(t *testing.T) {
			fx := newCancelFixture(t, true, store.RunStatusRunning)
			rp := &cancelReplacement{}
			tc.arm(fx, fx.replace(rp))
			got := actionDo(t, fx.hs.URL, cancelRow, fx.birth)
			wantOriginLost(t, "cloud cancel", got, actionRunID)
			fx.assertSuccessorKept(t, rp)
		})
	}
	t.Run("cloud-http-control-the-admitted-run", func(t *testing.T) {
		fx := newCancelFixture(t, true, store.RunStatusRunning)
		got := actionDo(t, fx.hs.URL, cancelRow, fx.birth)
		if got.status != http.StatusAccepted {
			t.Fatalf("cloud cancel at the current birth: %d %s, want 202", got.status, got.body)
		}
		a, err := fx.native.LoadRun(t.Context(), actionRunID)
		mustWS(t, err)
		if a.Status != store.RunStatusCancelled || len(fx.natsCancels()) != 1 {
			t.Fatalf("cloud cancel at the current birth: run %s, NATS cancels %v, want cancelled and one signal", a.Status, fx.natsCancels())
		}
	})
	// Legacy: without an expectation the flip runs on context.Background,
	// as at HEAD — it lands on whatever run holds the ID.
	t.Run("cloud-http-legacy-unchecked-flips-the-successor", func(t *testing.T) {
		fx := newCancelFixture(t, true, store.RunStatusRunning)
		rp := &cancelReplacement{}
		fx.flips.armFlip(fx.replace(rp))
		got := actionDo(t, fx.hs.URL, cancelRow, "")
		if got.status != http.StatusAccepted {
			t.Fatalf("unchecked cloud cancel: %d %s, want HEAD's 202", got.status, got.body)
		}
		fx.assertSuccessorCancelled(t, rp)
		if c := fx.natsCancels(); len(c) != 1 || c[0] != actionRunID {
			t.Fatalf("unchecked cloud cancel signalled %v, want one cancel of %s", c, actionRunID)
		}
	})
}

// sendCancel sends the cancel envelope.
func sendCancel(t *testing.T, c *websocket.Conn) {
	t.Helper()
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeCancel, AckID: "c1"})
}

// W13 (WS): on a socket the gate admitted on an expected birth the cancel is
// bound too. (iv) In process, A replaced after originHolds' bound load: the
// inactive flip refuses, and the socket answers run_origin_lost and closes
// scope_lost — never the ack HEAD swallowed the refusal into. (v) Cloud, A
// replaced inside the publisher's flip: the same, and no NATS cancel.
// Controls at the current birth: the ack, the run cancelled.
func TestRunWSCancelBindsTheFlip(t *testing.T) {
	t.Run("iv-local-inactive", func(t *testing.T) {
		fx := newCancelFixture(t, false, store.RunStatusPausedWaitingHuman)
		c := dialGate(t, fx.hs, "/api/ws/runs/"+actionRunID+"?expected_birth="+fx.birth)
		rp := &cancelReplacement{}
		fx.store.afterLoadN(1, fx.replace(rp))
		sendCancel(t, c)
		expectOriginLostClose(t, c, wsErrRunOriginLost)
		fx.assertSuccessorKept(t, rp)
	})
	t.Run("v-cloud-flip", func(t *testing.T) {
		fx := newCancelFixture(t, true, store.RunStatusRunning)
		c := dialGate(t, fx.hs, "/api/ws/runs/"+actionRunID+"?expected_birth="+fx.birth)
		rp := &cancelReplacement{}
		fx.flips.armFlip(fx.replace(rp))
		sendCancel(t, c)
		expectOriginLostClose(t, c, wsErrRunOriginLost)
		fx.assertSuccessorKept(t, rp)
	})
	for _, cloud := range []bool{false, true} {
		status := store.RunStatusPausedWaitingHuman
		if cloud {
			status = store.RunStatusRunning
		}
		t.Run(fmt.Sprintf("control-the-admitted-run/cloud=%t", cloud), func(t *testing.T) {
			fx := newCancelFixture(t, cloud, status)
			c := dialGate(t, fx.hs, "/api/ws/runs/"+actionRunID+"?expected_birth="+fx.birth)
			sendCancel(t, c)
			for _, env := range readUntilAck(t, c, "c1") {
				if env.Type == wsTypeError {
					t.Fatalf("cancel at the current birth answered an error before its ack: %s", env.Payload)
				}
			}
			a, err := fx.native.LoadRun(t.Context(), actionRunID)
			mustWS(t, err)
			if a.Status != store.RunStatusCancelled {
				t.Fatalf("cancel at the current birth left the run %s", a.Status)
			}
		})
	}
}

// W5 (the cancel envelope): on a socket admitted on an expected birth, a run
// service whose store does not endorse a bound observation is refused before
// the cancel acts — the scope_unavailable error, the run as it was.
func TestRunWSCancelRefusesAStoreThatDropsItsContext(t *testing.T) {
	fx := newCancelFixture(t, false, store.RunStatusPausedWaitingHuman)
	fx.srv.runs.Stop(t.Context())
	fx.srv.runs = newTestRunviewService(t, fx.srv.cfg.StoreDir, runview.WithRecoveryPassive(), runview.WithStore(nonEndorsingStore{fx.native}))
	c := dialGate(t, fx.hs, "/api/ws/runs/"+actionRunID+"?expected_birth="+fx.birth)
	sendCancel(t, c)
	env := readEnvelopeWithin(t, c, 5*time.Second, wsTypeError, wsTypeAck)
	if env.Type != wsTypeError || errorCodeOf(t, env).Code != errCodeScopeUnavailable {
		t.Fatalf("cancel over a non-endorsing store answered %s %s, want the %s error", env.Type, env.Payload, errCodeScopeUnavailable)
	}
	a, err := fx.native.LoadRun(t.Context(), actionRunID)
	mustWS(t, err)
	if a.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("a refused cancel left the run %s, want it paused as seeded", a.Status)
	}
}

// W4w (the cancel envelope): an unchecked socket cancels exactly as at HEAD
// — the swallow of the inactive flip's refusal and all. (a) A socket dialed
// without an expectation; (b) a socket dialed on one, the comparison then
// turned off (ITERION_RUN_BIRTH_CHECK=0, read per action). A replaced after
// originHolds' bound load: the flip lands on B and the socket acks.
func TestRunWSUncheckedCancelStaysUnbound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expect bool
		off    bool
	}{
		{"a-dialed-without-an-expectation", false, false},
		{"b-check-turned-off-after-the-dial", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCancelFixture(t, false, store.RunStatusPausedWaitingHuman)
			path := "/api/ws/runs/" + actionRunID
			if tc.expect {
				path += "?expected_birth=" + fx.birth
			}
			c := dialGate(t, fx.hs, path)
			if tc.off {
				t.Setenv(runBirthCheckEnv, "0")
			}
			rp := &cancelReplacement{}
			fx.store.afterLoadN(1, fx.replace(rp))
			sendCancel(t, c)
			for _, env := range readUntilAck(t, c, "c1") {
				if env.Type == wsTypeError {
					t.Fatalf("unchecked cancel answered an error before its ack: %s", env.Payload)
				}
			}
			fx.assertSuccessorCancelled(t, rp)
			// The ack is HEAD's; the socket stays open.
			if err := c.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatal("unchecked cancel sent a frame after its ack")
			} else if ce := (*websocket.CloseError)(nil); errors.As(err, &ce) {
				t.Fatalf("unchecked cancel closed the socket: %v", ce)
			}
		})
	}
}
