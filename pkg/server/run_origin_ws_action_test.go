package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The WS twins of the bound-mutation witnesses (plan §1 D5, P6). On a socket
// the gate admitted on an expected birth — and while the comparison is on —
// a mutating envelope's action runs under the socket's binding (carrier B),
// behind originHolds' bound load and a failure rule that answers a lost
// origin (carrier P), or carries the origin into the run service (the
// answer, carrier E). A socket dialed without an expectation, or any socket
// under ITERION_RUN_BIRTH_CHECK=0, makes exactly the calls it made before.

// wsActionRow is one mutating envelope over the run its HTTP twin (an
// actionRows row) seeds, A and B alike.
type wsActionRow struct {
	typ     string
	row     string
	payload string
	// answer is the envelope's reply at the run's current birth: "" an ack,
	// otherwise the error code it answers.
	answer string
	// bound reports that the action reaches the store after originHolds'
	// load (a write, a service pre-check); an ID-keyed signal alone does not.
	bound bool
}

func wsActionRows() []wsActionRow {
	return []wsActionRow{
		{typ: wsTypeQueueMessage, row: "queue-message", payload: `{"text":"meant for A"}`, bound: true},
		{typ: wsTypeCancelQueuedMessage, row: "cancel-queued-message", payload: `{"message_id":"m1"}`, bound: true},
		{typ: wsTypeCancel, row: "cancel-inactive", bound: true},
		{typ: wsTypePause, row: "pause", answer: "not_active"},
		{typ: wsTypeBumpLoop, row: "bump-loop", payload: `{"loop_name":"l","delta":1}`, answer: "not_active", bound: true},
		{typ: wsTypeRaiseBudget, row: "raise-budget", payload: `{"max_tokens":100}`, answer: "not_active", bound: true},
	}
}

func (r wsActionRow) envelope(ackID string) runWSEnvelope {
	env := runWSEnvelope{Type: r.typ, AckID: ackID}
	if r.payload != "" {
		env.Payload = json.RawMessage(r.payload)
	}
	return env
}

// wsAnswerEnvelope answers the human gate of the expect fixture's
// answer-human run.
func wsAnswerEnvelope(ackID string) runWSEnvelope {
	return runWSEnvelope{Type: wsTypeAnswer, AckID: ackID, Payload: json.RawMessage(`{"answers":{"ok":true}}`)}
}

// wsRunPath is the run socket of actionRunID, dialed on birth ("" dials
// without an expectation, as an older client does).
func wsRunPath(birth string) string {
	path := "/api/ws/runs/" + actionRunID
	if birth != "" {
		path += "?" + expectedBirthParam + "=" + birth
	}
	return path
}

// readReply reads up to the envelope answering ackID — its ack or its error
// — and fails on any other error before it.
func readReply(t *testing.T, c *websocket.Conn, ackID string) runWSEnvelope {
	t.Helper()
	for {
		env := readEnvelopeWithin(t, c, 10*time.Second)
		if env.AckID == ackID && (env.Type == wsTypeAck || env.Type == wsTypeError) {
			return env
		}
		if env.Type == wsTypeError {
			t.Fatalf("an error before the reply to %s: %s", ackID, env.Payload)
		}
	}
}

// wantReply requires the reply HEAD gives: an ack, or the error code.
func wantReply(t *testing.T, label string, env runWSEnvelope, answer string) {
	t.Helper()
	switch {
	case answer == "" && env.Type != wsTypeAck:
		t.Fatalf("%s: %s %s, want an ack", label, env.Type, env.Payload)
	case answer != "" && (env.Type != wsTypeError || errorCodeOf(t, env).Code != answer):
		t.Fatalf("%s: %s %s, want the %s error", label, env.Type, env.Payload, answer)
	}
}

// wantSocketOpen requires that nothing follows the reply: no frame, no close.
func wantSocketOpen(t *testing.T, c *websocket.Conn, label string) {
	t.Helper()
	mustWS(t, c.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, raw, err := c.ReadMessage()
	if err == nil {
		t.Fatalf("%s sent a frame after its reply: %s", label, raw)
	}
	if ce := (*websocket.CloseError)(nil); errors.As(err, &ce) {
		t.Fatalf("%s closed the socket: %v", label, ce)
	}
}

// The act rung of a socket with a credential (the DisableAuth chain stamps
// the dev-mode one) reads the run twice before the envelope's handler:
// originHolds' bound load, then the run's team (runTenant, unbound).
const wsActRungLoads = 2

// assertWSBoundUntilRefused is the site half of a refused envelope whose run
// was replaced right after originHolds' load: the act rung's team read
// comes first (unbound), the failure rule's confirm read last (unbound), and
// every access between them — the action's — carries the binding. An
// ID-keyed signal makes no access between them.
func assertWSBoundUntilRefused(t *testing.T, fx *actionFixture, row wsActionRow) {
	t.Helper()
	calls, _ := fx.store.probe.recorded()
	n := len(calls)
	if n < 2 || calls[0].method != "LoadRun" || calls[0].bound || calls[n-1].method != "LoadRun" || calls[n-1].bound {
		t.Fatalf("%s after the replacement: %+v, want the team read first and the confirm read last, both unbound", row.typ, calls)
	}
	action := calls[1 : n-1]
	for _, c := range action {
		if !c.bound {
			t.Fatalf("%s made an unbound access after the replacement: %+v (all: %+v)", row.typ, c, calls)
		}
	}
	if row.bound != (len(action) > 0) {
		t.Fatalf("%s made %d accesses between the team read and the confirm read: %+v", row.typ, len(action), calls)
	}
}

// W10: on a socket admitted on A's birth, every mutating envelope whose run
// is replaced by an actionable B right after originHolds' bound load is
// refused — run_origin_lost, then scope_lost — and B's run directory is as
// the replacement left it: no message queued or cancelled, no status
// flipped. The store accesses of a bound action carry the binding.
func TestRunWSOriginMutationBoundToTheOrigin(t *testing.T) {
	for _, row := range wsActionRows() {
		t.Run(row.typ, func(t *testing.T) {
			fx := newActionFixture(t, actionRowByName(t, row.row))
			c := dialGate(t, fx.hs, wsRunPath(fx.birth))
			pic := fx.armReplacement(t, 1)
			writeJSONMessage(t, c, row.envelope("m"))
			expectOriginLostClose(t, c, wsErrRunOriginLost)
			fx.assertSuccessorUntouched(t, pic)
			assertWSBoundUntilRefused(t, fx, row)
		})
	}
}

// W10h: on a socket admitted on the run's current birth, an envelope whose
// action fails on the healthy run — the pause, the loop bump and the budget
// raise of a run no execution of this process holds — answers its own
// refusal (not_active), never run_origin_lost, and the socket stays open: the
// failure rule reads the run again and finds the admitted birth.
func TestRunWSFailureOnTheAdmittedRunKeepsTheSocket(t *testing.T) {
	for _, row := range wsActionRows() {
		if row.answer == "" {
			continue
		}
		t.Run(row.typ, func(t *testing.T) {
			fx := newActionFixture(t, actionRowByName(t, row.row))
			c := dialGate(t, fx.hs, wsRunPath(fx.birth))
			writeJSONMessage(t, c, row.envelope("m"))
			wantReply(t, row.typ+" on the admitted run", readReply(t, c, "m"), row.answer)
			wantSocketOpen(t, c, row.typ+" on the admitted run")
			if got := birthOf(t, fx.native, actionRunID); got != fx.birth {
				t.Fatalf("fixture: the run's birth moved to %q", got)
			}
		})
	}
}

// W11: the answer envelope on a socket admitted on A's birth, A replaced by
// B — paused on the same human gate — right after the envelope's own load
// (after the act rung's two). The resume carries the socket's origin, so the
// run service refuses B: run_origin_lost, then scope_lost; B's run directory
// is as the replacement left it and nothing was resumed. Replaced later —
// right after the run service's check at the top of its resume, or on entry
// of the engine's own load after the check under the spawn lock — the run
// service's check before its first effect on the run's directory, then the
// engine, refuse B the same way, and B's directory gains no entry. A deleted
// after the act rung and before the envelope's load answers the same loss.
func TestRunWSAnswerCarriesTheOrigin(t *testing.T) {
	for _, at := range []struct {
		name string
		arm  func(fx *expectFixture, replace func() error)
	}{
		{"replaced-after-the-service-check", afterServiceCheck},
		{"replaced-at-the-engine-load", atEngineLoad},
	} {
		t.Run(at.name, func(t *testing.T) {
			row := expectRowByName(t, "answer-human")
			row.arm = at.arm
			fx := newExpectFixture(t, row)
			c := dialGate(t, fx.hs, wsRunPath(fx.birth))
			pic := fx.armExpectReplacement(t)
			writeJSONMessage(t, c, wsAnswerEnvelope("a1"))
			expectOriginLostClose(t, c, wsErrRunOriginLost)
			fx.settle(t)
			fx.assertSuccessorUnresumed(t, pic)
		})
	}
	t.Run("replaced-after-the-envelope-load", func(t *testing.T) {
		row := expectRowByName(t, "answer-human")
		fx := newExpectFixture(t, row)
		c := dialGate(t, fx.hs, wsRunPath(fx.birth))
		fx.row.arm = afterLoad(wsActRungLoads + 1)
		pic := fx.armExpectReplacement(t)
		writeJSONMessage(t, c, wsAnswerEnvelope("a1"))
		expectOriginLostClose(t, c, wsErrRunOriginLost)
		fx.settle(t)
		mustWS(t, fx.store.failure())
		pic.mu.Lock()
		defer pic.mu.Unlock()
		if pic.birth == "" || pic.birth == fx.birth {
			t.Fatalf("the replacement did not run: successor birth %q, original %q", pic.birth, fx.birth)
		}
		if d := diffDigests(pic.run, mustTreeDigest(t, filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID))); len(d) != 0 {
			t.Fatalf("the successor's run directory changed:\n%s", strings.Join(d, "\n"))
		}
		b, err := fx.native.LoadRun(t.Context(), actionRunID)
		mustWS(t, err)
		if b.Status != store.RunStatusPausedWaitingHuman {
			t.Fatalf("the successor is %s, want it paused on its gate as seeded", b.Status)
		}
	})
	t.Run("deleted-before-the-envelope-load", func(t *testing.T) {
		fx := newExpectFixture(t, expectRowByName(t, "answer-human"))
		c := dialGate(t, fx.hs, wsRunPath(fx.birth))
		fx.store.afterLoadN(wsActRungLoads, func() error {
			ctx := context.Background()
			if err := fx.native.UpdateRunStatus(ctx, actionRunID, store.RunStatusFinished, ""); err != nil {
				return err
			}
			return fx.native.DeleteRun(ctx, actionRunID)
		})
		writeJSONMessage(t, c, wsAnswerEnvelope("a1"))
		expectOriginLostClose(t, c, wsErrRunOriginLost)
		mustWS(t, fx.store.failure())
	})
}

// T6': on a socket admitted on the run's current birth the mutating
// envelopes serve the run: queue_message and the inactive cancel ack and
// land, every probed store access after the act rung bound to the socket's
// origin; the answer resumes the run, and nothing after originHolds' load —
// the envelope's reads, the execution's accesses — carries the binding.
func TestRunWSOriginMutationsBindOnAnExpectation(t *testing.T) {
	for _, name := range []string{"queue-message", "cancel-inactive"} {
		t.Run("writes/"+name, func(t *testing.T) {
			var row wsActionRow
			for _, r := range wsActionRows() {
				if r.row == name {
					row = r
				}
			}
			fx := newActionFixture(t, actionRowByName(t, name))
			c := dialGate(t, fx.hs, wsRunPath(fx.birth))
			fx.store.afterLoadN(wsActRungLoads, func() error { fx.store.probe.reset(); return nil })
			writeJSONMessage(t, c, row.envelope("m"))
			wantReply(t, row.typ, readReply(t, c, "m"), "")
			calls, landed := fx.store.probe.recorded()
			if len(landed) == 0 {
				t.Fatalf("%s at the current birth landed no write (calls %+v)", row.typ, calls)
			}
			for _, call := range calls {
				if !call.bound {
					t.Fatalf("%s at the current birth made an unbound access: %+v (all: %+v)", row.typ, call, calls)
				}
			}
			a, err := fx.native.LoadRun(t.Context(), actionRunID)
			mustWS(t, err)
			msgs, err := fx.native.ListQueuedMessages(t.Context(), actionRunID)
			mustWS(t, err)
			switch name {
			case "queue-message":
				if len(msgs) != 1 || msgs[0].Text != "meant for A" {
					t.Fatalf("queued = %+v, want the envelope's message", msgs)
				}
			case "cancel-inactive":
				if a.Status != store.RunStatusCancelled {
					t.Fatalf("status = %s, want cancelled", a.Status)
				}
			}
		})
	}
	t.Run("answer-execution-unbound", func(t *testing.T) {
		fx := newExpectFixture(t, expectRowByName(t, "answer-human"))
		c := dialGate(t, fx.hs, wsRunPath(fx.birth))
		// originHolds' load is bound by design; everything after it is the
		// envelope's and the execution's.
		fx.store.afterLoadN(1, func() error { fx.store.probe.reset(); return nil })
		writeJSONMessage(t, c, wsAnswerEnvelope("a1"))
		wantReply(t, "answer", readReply(t, c, "a1"), "")
		fx.settle(t)
		waitRunSettled(t, fx.native, actionRunID)
		mustWS(t, fx.store.failure())
		calls, _ := fx.store.probe.recorded()
		executed := false
		for _, call := range calls {
			if call.bound {
				t.Fatalf("the answer or its execution made a bound access: %+v", call)
			}
			if call.method == "AppendEvent" || call.method == "SaveCheckpoint" {
				executed = true
			}
		}
		if !executed {
			t.Fatalf("fixture: the resumed execution made no recorded access: %+v", calls)
		}
		a, err := fx.native.LoadRun(t.Context(), actionRunID)
		mustWS(t, err)
		if a.Status == store.RunStatusPausedWaitingHuman {
			t.Fatalf("the answer did not resume the run: %s", a.Status)
		}
	})
}

// wsLegacyPicture is what an unchecked envelope did around the replacement:
// the accesses before it (originHolds' load) and, through the probe, after.
type wsLegacyPicture struct {
	mu    sync.Mutex
	pre   []storeCall
	birth string
}

// armLegacyReplacement replaces A by B right after the nth run load from now,
// keeping the accesses recorded before the hook.
func armLegacyReplacement(fx *gateFixture, n int32, replace func(context.Context) error) *wsLegacyPicture {
	pic := &wsLegacyPicture{}
	fx.store.afterLoadN(n, func() error {
		pre, _ := fx.store.probe.recorded()
		ctx := context.Background()
		if err := replace(ctx); err != nil {
			return err
		}
		b, err := fx.native.LoadRun(ctx, actionRunID)
		if err != nil {
			return err
		}
		pic.mu.Lock()
		pic.pre, pic.birth = pre, b.BirthID
		pic.mu.Unlock()
		fx.store.probe.reset()
		return nil
	})
	return pic
}

// assertLegacyCalls: the replacement ran; the envelope's only bound access
// was originHolds' one load, and every access after it was unbound.
func (pic *wsLegacyPicture) assertLegacyCalls(t *testing.T, fx *gateFixture, original string) {
	t.Helper()
	mustWS(t, fx.store.failure())
	pic.mu.Lock()
	defer pic.mu.Unlock()
	if pic.birth == "" || pic.birth == original {
		t.Fatalf("the replacement did not run: successor birth %q, original %q", pic.birth, original)
	}
	if len(pic.pre) < 1 || pic.pre[0].method != "LoadRun" || !pic.pre[0].bound {
		t.Fatalf("before the replacement: %+v, want originHolds' bound load first", pic.pre)
	}
	for _, c := range pic.pre[1:] {
		if c.bound {
			t.Fatalf("a bound access besides originHolds' load: %+v (all: %+v)", c, pic.pre)
		}
	}
	calls, _ := fx.store.probe.recorded()
	for _, c := range calls {
		if c.bound {
			t.Fatalf("an unchecked envelope made a bound access: %+v", c)
		}
	}
}

// legacySocket dials the case's socket: (a) without an expectation, (b) on
// A's birth with the comparison turned off after the dial
// (ITERION_RUN_BIRTH_CHECK=0, read per action).
func legacySocket(t *testing.T, fx *gateFixture, birth string, off bool) *websocket.Conn {
	t.Helper()
	path := wsRunPath("")
	if off {
		path = wsRunPath(birth)
	}
	c := dialGate(t, fx.hs, path)
	if off {
		t.Setenv(runBirthCheckEnv, "0")
	}
	fx.store.probe.reset()
	return c
}

// W4w: the legacy contract of every mutating envelope (the cancel's is
// TestRunWSUncheckedCancelStaysUnbound). (a) A socket dialed without an
// expectation; (b) a socket dialed on one, the comparison then turned off.
// A replaced by B after originHolds' bound load — after the answer's own
// load for the answer: each envelope acts exactly as at HEAD — the message
// lands in B's inbox, B's queued message is cancelled, the steering and the
// pause answer their own refusal, the answer resumes B — with no bound
// access but originHolds' load, and the socket stays open.
func TestRunWSUncheckedMutationsStayUnbound(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  bool
	}{
		{"a-dialed-without-an-expectation", false},
		{"b-check-turned-off-after-the-dial", true},
	} {
		for _, row := range wsActionRows() {
			if row.typ == wsTypeCancel {
				continue
			}
			t.Run(tc.name+"/"+row.typ, func(t *testing.T) {
				fx := newActionFixture(t, actionRowByName(t, row.row))
				c := legacySocket(t, fx.gateFixture, fx.birth, tc.off)
				pic := armLegacyReplacement(fx.gateFixture, 1, func(ctx context.Context) error {
					return replaceActionRun(ctx, fx.native, fx.row, fx.env, "B")
				})
				writeJSONMessage(t, c, row.envelope("m"))
				wantReply(t, row.typ+" unchecked", readReply(t, c, "m"), row.answer)
				pic.assertLegacyCalls(t, fx.gateFixture, fx.birth)
				msgs, err := fx.native.ListQueuedMessages(t.Context(), actionRunID)
				mustWS(t, err)
				switch row.typ {
				case wsTypeQueueMessage:
					if len(msgs) != 1 || msgs[0].Text != "meant for A" {
						t.Fatalf("unchecked queue_message: B's inbox %+v, want HEAD's append on B", msgs)
					}
				case wsTypeCancelQueuedMessage:
					if len(msgs) != 1 || msgs[0].Status != store.QueuedMessageStatusCancelled {
						t.Fatalf("unchecked cancel_queued_message: B's inbox %+v, want HEAD's cancel on B", msgs)
					}
				}
				wantSocketOpen(t, c, row.typ+" unchecked")
			})
		}
		t.Run(tc.name+"/"+wsTypeAnswer, func(t *testing.T) {
			fx := newExpectFixture(t, expectRowByName(t, "answer-human"))
			c := legacySocket(t, fx.gateFixture, fx.birth, tc.off)
			pic := armLegacyReplacement(fx.gateFixture, wsActRungLoads+1, fx.replaceExpectRun)
			writeJSONMessage(t, c, wsAnswerEnvelope("a1"))
			wantReply(t, "answer unchecked", readReply(t, c, "a1"), "")
			fx.settle(t)
			waitRunSettled(t, fx.native, actionRunID)
			pic.assertLegacyCalls(t, fx.gateFixture, fx.birth)
			b, err := fx.native.LoadRun(t.Context(), actionRunID)
			mustWS(t, err)
			if b.BirthID != pic.birth || b.Status == store.RunStatusPausedWaitingHuman {
				t.Fatalf("unchecked answer: the run is %s at birth %q, want HEAD's resume of the successor %q", b.Status, b.BirthID, pic.birth)
			}
			wantSocketOpen(t, c, "answer unchecked")
		})
	}
}

// W5 (the WS envelopes): on a socket admitted on an expected birth, a run
// service whose store does not endorse a bound observation is refused before
// any envelope acts — the scope_unavailable error, the run as it was (the
// cancel's is TestRunWSCancelRefusesAStoreThatDropsItsContext).
func TestRunWSActionsRefuseAStoreThatDropsItsContext(t *testing.T) {
	for _, row := range wsActionRows() {
		if row.typ == wsTypeCancel {
			continue
		}
		t.Run(row.typ, func(t *testing.T) {
			fx := newActionFixture(t, actionRowByName(t, row.row))
			fx.srv.runs.Stop(t.Context())
			fx.srv.runs = newTestRunviewService(t, fx.srv.cfg.StoreDir, runview.WithRecoveryPassive(), runview.WithStore(nonEndorsingStore{fx.native}))
			runDir := filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID)
			c := dialGate(t, fx.hs, wsRunPath(fx.birth))
			before := mustTreeDigest(t, runDir)
			writeJSONMessage(t, c, row.envelope("m"))
			wantReply(t, row.typ+" over a non-endorsing store", readReply(t, c, "m"), errCodeScopeUnavailable)
			if d := diffDigests(before, mustTreeDigest(t, runDir)); len(d) != 0 {
				t.Fatalf("%s acted though refused:\n%s", row.typ, strings.Join(d, "\n"))
			}
		})
	}
	t.Run(wsTypeAnswer, func(t *testing.T) {
		fx := newExpectFixture(t, expectRowByName(t, "answer-human"))
		fx.srv.runs.Stop(t.Context())
		fx.srv.runs = newTestRunviewService(t, fx.srv.cfg.StoreDir, runview.WithRecoveryPassive(), runview.WithStore(nonEndorsingStore{fx.native}))
		runDir := filepath.Join(fx.srv.cfg.StoreDir, "runs", actionRunID)
		c := dialGate(t, fx.hs, wsRunPath(fx.birth))
		before := mustTreeDigest(t, runDir)
		writeJSONMessage(t, c, wsAnswerEnvelope("a1"))
		wantReply(t, "answer over a non-endorsing store", readReply(t, c, "a1"), errCodeScopeUnavailable)
		fx.settle(t)
		if d := diffDigests(before, mustTreeDigest(t, runDir)); len(d) != 0 {
			t.Fatalf("the answer acted though refused:\n%s", strings.Join(d, "\n"))
		}
	})
}
