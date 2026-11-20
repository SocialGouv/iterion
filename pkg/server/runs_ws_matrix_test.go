package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run socket's origin matrix on the production local wiring (FS native).
// A socket binds the run its upgrade read; every snapshot, event and log
// subscription of that socket reads that run or is refused typed, whatever
// the order of the subscriptions and wherever the replacement lands.

// W0: the matrix fixture serves the native merge through the hook source;
// the same probe on a fixture rebuilt over a self-endorsing wrapper store —
// the legacy stream path — sees the broker payload, so the probe tells the
// two wirings apart.
func TestRunWSMatrixFixtureIsTheProductionWiring(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		fx := newMatrixServer(t, matrixOptions{})
		if src, ok := fx.srv.runs.StreamSource().(*matrixHookSource); !ok || src != fx.hook {
			t.Fatalf("the run console's stream source is %T, want the fixture's hook source", fx.srv.runs.StreamSource())
		}
	})
	t.Run("legacy-control", func(t *testing.T) {
		fx := buildMatrixServer(t, matrixOptions{legacy: true})
		if !fx.wiringProbe(t) {
			t.Fatal("on the legacy stream path the wiring probe saw no BROKER-PROBE: it cannot tell the wirings apart")
		}
	})
}

// W1 (R1): a run replaced, or its tenant rewritten under its birth, before a
// snapshot — on the first subscribe of an unchecked dial, on a resubscribe,
// and after a checked dial — is refused: snapshot_failed, then scope_lost, no
// snapshot frame, and no event subscription reaches the source. Legitimate
// writes under the same birth keep serving the run.
func TestRunWSMatrixReplacedBeforeTheSnapshot(t *testing.T) {
	for _, f := range matrixFormats {
		t.Run(fmt.Sprintf("f%d/first-subscribe", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 1))
			c := dialRunWS(t, fx.hs, "r")
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", matrixSuccessor, f, store.RunStatusFinished, 1))
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			expectMatrixOriginLost(t, c, "snapshot_failed", matrixSuccessor)
			if n := fx.hook.eventSubscriptions("r"); n != 0 {
				t.Fatalf("event subscriptions of r = %d, want none: a refused snapshot reached the source", n)
			}
		})
		t.Run(fmt.Sprintf("f%d/resubscribe", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 1))
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != matrixOrigin {
				t.Fatalf("first snapshot names %q", snap.Run.WorkflowName)
			}
			readEnvelopeWithin(t, c, 10*time.Second, wsTypeTerminated)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
			readUntilAck(t, c, "u1")
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", matrixSuccessor, f, store.RunStatusFinished, 1))
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s2"})
			expectMatrixOriginLost(t, c, "snapshot_failed", matrixSuccessor)
			if n := fx.hook.eventSubscriptions("r"); n != 1 {
				t.Fatalf("event subscriptions of r = %d, want only the first", n)
			}
		})
		t.Run(fmt.Sprintf("f%d/checked-dial", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 1))
			c := dialGate(t, fx.hs, "/api/ws/runs/r?"+expectedBirthParam+"="+url.QueryEscape(birthOf(t, fx.native, "r")))
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", matrixSuccessor, f, store.RunStatusFinished, 1))
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			expectMatrixOriginLost(t, c, "snapshot_failed", matrixSuccessor)
			if n := fx.hook.eventSubscriptions("r"); n != 0 {
				t.Fatalf("event subscriptions of r = %d, want none: a refused snapshot reached the source", n)
			}
		})
		t.Run(fmt.Sprintf("f%d/tenant", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 1))
			origin, err := runview.CaptureRunOriginIn(ctx, fx.native, "r")
			mustWS(t, err)
			retenantMatrixRun(t, fx, "r", "T", origin.Header())
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != matrixOrigin {
				t.Fatalf("first snapshot names %q", snap.Run.WorkflowName)
			}
			readEnvelopeWithin(t, c, 10*time.Second, wsTypeTerminated)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
			readUntilAck(t, c, "u1")
			retenantMatrixRun(t, fx, "r", "U", origin.Header())
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s2"})
			expectMatrixOriginLost(t, c, "snapshot_failed", `"type":"snapshot"`)
			if n := fx.hook.eventSubscriptions("r"); n != 1 {
				t.Fatalf("event subscriptions of r = %d, want only the first", n)
			}
		})
		t.Run(fmt.Sprintf("f%d/legitimate-writes", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusRunning, 1))
			before, err := fx.native.LoadRun(ctx, "r")
			mustWS(t, err)
			c := dialRunWS(t, fx.hs, "r")
			resubscribe := func(step string) {
				t.Helper()
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s-" + step})
				if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.ID != "r" || snap.Run.WorkflowName != matrixOrigin {
					t.Fatalf("%s: snapshot %+v", step, snap.Run)
				}
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u-" + step})
				readUntilAck(t, c, "u-"+step)
			}
			resubscribe("start")
			// The writers carry the run's own format intent, as the engine's do.
			write, err := store.BindRunValueFormat(ctx, before)
			mustWS(t, err)
			r, err := fx.native.LoadRun(ctx, "r")
			mustWS(t, err)
			r.Name = "renamed"
			mustWS(t, fx.native.SaveRun(write, r))
			resubscribe("save")
			mustWS(t, fx.native.PauseRun(write, "r", &store.Checkpoint{}))
			resubscribe("pause")
			mustWS(t, fx.native.UpdateRunStatus(write, "r", store.RunStatusRunning, ""))
			resubscribe("resume")
			after, err := fx.native.LoadRun(ctx, "r")
			mustWS(t, err)
			if after.ExecutionEpoch == before.ExecutionEpoch || after.BirthID != before.BirthID {
				t.Fatalf("fixture did not rotate the epoch under one birth: %q→%q, birth %q→%q", before.ExecutionEpoch, after.ExecutionEpoch, before.BirthID, after.BirthID)
			}
			mustWS(t, fx.native.UpdateRunStatus(write, "r", store.RunStatusFinished, ""))
			resubscribe("finished")
		})
	}
}

// W2 (R2): a run replaced after the snapshot frame was queued and before the
// event subscription reaches the Service is refused by the retained
// subscription: event_stream_failed, then scope_lost, no frame of the
// successor, with or without the replay.
func TestRunWSMatrixReplacedBetweenSnapshotAndEvents(t *testing.T) {
	for _, f := range matrixFormats {
		for _, replay := range []bool{true, false} {
			t.Run(fmt.Sprintf("f%d/replay=%t", f, replay), func(t *testing.T) {
				fx := newMatrixServer(t, matrixOptions{})
				ctx := t.Context()
				mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 2))
				fx.hook.beforeNextEvents("r", func() error {
					// More events than A: seqs past A's last are past any cursor.
					return replaceWSOriginRun(context.Background(), fx.native, "r", matrixSuccessor, f, store.RunStatusFinished, 4)
				})
				c := dialRunWS(t, fx.hs, "r")
				payload := json.RawMessage(`{}`)
				if replay {
					payload = json.RawMessage(`{"replay_history":true,"from_seq":0}`)
				}
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s", Payload: payload})
				if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != matrixOrigin {
					t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
				}
				expectMatrixOriginLost(t, c, "event_stream_failed", matrixSuccessor)
				mustWS(t, fx.hook.failure())
				if b, err := fx.native.LoadRun(ctx, "r"); err != nil || b.WorkflowName != matrixSuccessor {
					t.Fatalf("the replacement hook did not run: %+v %v", b, err)
				}
			})
		}
	}
}

// W3 (R5): one socket, both orders of the two subscription kinds, the run
// replaced between them. The binding is the upgrade's, not the first
// subscription's: the second kind is refused whichever came first.
func TestRunWSMatrixLogsAndEventsInBothOrders(t *testing.T) {
	const originLog = matrixOrigin + " log\n"
	for _, f := range matrixFormats {
		t.Run(fmt.Sprintf("f%d/logs-then-events", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 1))
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", originLog)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
			readUntilAck(t, c, "l1")
			if got := collectLogText(t, c, "", originLog, 5*time.Second); got != originLog {
				t.Fatalf("logs of the origin = %q", got)
			}
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribeLog, AckID: "ul1"})
			readUntilAck(t, c, "ul1")
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", matrixSuccessor, f, store.RunStatusFinished, 1))
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", matrixSuccessor+" log\n")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			expectMatrixOriginLost(t, c, "snapshot_failed", matrixSuccessor)
		})
		t.Run(fmt.Sprintf("f%d/events-then-logs", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 1))
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", originLog)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != matrixOrigin {
				t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
			}
			readEnvelopeWithin(t, c, 10*time.Second, wsTypeTerminated)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
			readUntilAck(t, c, "u1")
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", matrixSuccessor, f, store.RunStatusFinished, 1))
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", matrixSuccessor+" log\n")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
			expectMatrixOriginLost(t, c, "log_stream_failed", matrixSuccessor)
		})
		// The log twin of W2: the run replaced inside the log subscribe,
		// before it reaches the Service, is refused at the log admission.
		t.Run(fmt.Sprintf("f%d/replaced-inside-the-log-subscribe", f), func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, f, store.RunStatusFinished, 1))
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", originLog)
			fx.hook.beforeNextLogs("r", func() error {
				if err := replaceWSOriginRun(context.Background(), fx.native, "r", matrixSuccessor, f, store.RunStatusFinished, 1); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(fx.srv.cfg.StoreDir, "runs", "r", "run.log"), []byte(matrixSuccessor+" log\n"), 0o600)
			})
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
			expectMatrixOriginLost(t, c, "log_stream_failed", matrixSuccessor)
			mustWS(t, fx.hook.failure())
			if n := fx.hook.logSubscriptions("r"); n != 1 {
				t.Fatalf("log subscriptions of r = %d, want the hooked one", n)
			}
		})
	}
}

// W4 (R6): at the unsubscribe ack the merge's broker subscription is gone, and
// a resubscribe of either kind serves the origin from its cursor exactly once;
// after a replacement a resubscribe of each kind is refused typed. The wire
// sees a missing join only when the ack outruns the release, about half the
// time: the join before the ack is witnessed deterministically by
// TestWSOwnerUnsubscribeJoinsBothPumps (the socket's unsubscribe) and
// TestLiveEventsSubscribeAndCloseAreSynchronous (the merge's close).
func TestRunWSMatrixUnsubscribeReleasesAndResubscribeServesTheOrigin(t *testing.T) {
	fx := newMatrixServer(t, matrixOptions{})
	ctx := t.Context()
	storeDir := fx.srv.cfg.StoreDir
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, store.LegacyValueFormat, store.RunStatusRunning, 1))
	const firstLog, secondLog = "x-bytes\n", "y-bytes\n"
	logPath := writeRunLog(t, storeDir, "r", firstLog)
	c := dialRunWS(t, fx.hs, "r")
	idle := dialRunWS(t, fx.hs, "r")
	b := fx.srv.runs.Broker()

	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
	snapshotOf(t, readEnvelope(t, c))
	awaitBrokerSubscriber(t, fx.srv, "r")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
	readUntilAck(t, c, "u1")
	if n := b.SubscriberCount("r"); n != 0 {
		t.Fatalf("broker subscribers of r at the unsubscribe ack = %d, want 0: the stream's broker subscription outlived its ack", n)
	}
	r, err := fx.native.LoadRun(ctx, "r")
	mustWS(t, err)
	write, err := store.BindRunValueFormat(ctx, r)
	mustWS(t, err)
	appendLive := func(node string) *store.Event {
		t.Helper()
		ev, err := fx.native.AppendEvent(write, "r", store.Event{Type: store.EventNodeFinished, NodeID: node})
		mustWS(t, err)
		b.Publish(*ev)
		return ev
	}
	missed := appendLive("after-unsubscribe")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s2", Payload: json.RawMessage(fmt.Sprintf(`{"replay_history":true,"from_seq":%d}`, missed.Seq))})
	// Nothing of the unsubscribed stream follows the resubscribe: its first
	// frame is its own snapshot.
	if env := readEnvelope(t, c); env.Type != wsTypeSnapshot {
		t.Fatalf("first frame after the resubscribe = %s %s, want its snapshot", env.Type, env.Payload)
	}
	awaitBrokerSubscriber(t, fx.srv, "r")
	appendLive("live-fence")
	seen := map[string]int{}
	for seen["live-fence"] == 0 {
		for _, ev := range decodeEventEnvelope(t, readEnvelopeWithin(t, c, 5*time.Second, wsTypeEvent, wsTypeEventBatch)) {
			seen[ev.NodeID]++
		}
	}
	if seen["after-unsubscribe"] != 1 || seen["live-fence"] != 1 {
		t.Fatalf("resubscribed from seq %d: events %v, want the missed event and the fence once each", missed.Seq, seen)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u2"})
	readUntilAck(t, c, "u2")

	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
	readUntilAck(t, c, "l1")
	if got := collectLogText(t, c, "", firstLog, 5*time.Second); got != firstLog {
		t.Fatalf("first log subscription = %q", got)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribeLog, AckID: "ul1"})
	readUntilAck(t, c, "ul1")
	appendRunLog(t, logPath, secondLog)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l2", Payload: json.RawMessage(fmt.Sprintf(`{"from_offset":%d}`, len(firstLog)))})
	readUntilAck(t, c, "l2")
	if got := readMatrixLogSpan(t, c, int64(len(firstLog)), len(secondLog)); got != secondLog {
		t.Fatalf("log resubscribed from offset %d = %q, want exactly %q", len(firstLog), got, secondLog)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribeLog, AckID: "ul2"})
	readUntilAck(t, c, "ul2")

	// A running run is replaced aside, as an external writer would.
	mustWS(t, os.Rename(filepath.Join(storeDir, "runs", "r"), filepath.Join(storeDir, "aside-r")))
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixSuccessor, store.LegacyValueFormat, store.RunStatusRunning, 2))
	writeRunLog(t, storeDir, "r", matrixSuccessor+" log\n")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s3"})
	expectMatrixOriginLost(t, c, "snapshot_failed", matrixSuccessor)
	writeJSONMessage(t, idle, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l3"})
	expectMatrixOriginLost(t, idle, "log_stream_failed", matrixSuccessor)
}

// readMatrixLogSpan reads log chunks of n bytes from offset from: each chunk
// must start where the previous one ended, the first at from.
func readMatrixLogSpan(t *testing.T, c *websocket.Conn, from int64, n int) string {
	t.Helper()
	var got strings.Builder
	next := from
	for got.Len() < n {
		env := readEnvelopeWithin(t, c, 5*time.Second, wsTypeLogChunk, wsTypeLogTerminated)
		if env.Type == wsTypeLogTerminated {
			t.Fatalf("log ended after %q", got.String())
		}
		p := decodeLogChunk(t, env)
		if p.Offset != next {
			t.Fatalf("log chunk at offset %d (%q), want %d: a byte before the cursor or a gap", p.Offset, p.Text, next)
		}
		got.WriteString(p.Text)
		next += int64(len(p.Text))
	}
	return got.String()
}

// matrixWire collects a socket's event frames: the persisted events' seqs in
// arrival order and the alert frames by node ID.
type matrixWire struct {
	seqs   []int64
	alerts map[string]int
}

func newMatrixWire() *matrixWire { return &matrixWire{alerts: map[string]int{}} }

func (w *matrixWire) has(seq int64) bool {
	for _, s := range w.seqs {
		if s == seq {
			return true
		}
	}
	return false
}

// read consumes one frame; it reports whether the stream terminated.
func (w *matrixWire) read(t *testing.T, c *websocket.Conn) bool {
	t.Helper()
	env := readEnvelopeWithin(t, c, 10*time.Second, wsTypeEvent, wsTypeEventBatch, wsTypeTerminated, wsTypeError)
	switch env.Type {
	case wsTypeTerminated:
		return true
	case wsTypeError:
		t.Fatalf("the stream failed: %s", env.Payload)
	}
	for _, ev := range decodeEventEnvelope(t, env) {
		if ev.Type == store.EventAlert {
			w.alerts[ev.NodeID]++
			continue
		}
		w.seqs = append(w.seqs, ev.Seq)
	}
	return false
}

func (w *matrixWire) readUntil(t *testing.T, c *websocket.Conn, done func() bool) {
	t.Helper()
	for !done() {
		if w.read(t, c) {
			t.Fatalf("the stream terminated early: seqs %v alerts %v", w.seqs, w.alerts)
		}
	}
}

func (w *matrixWire) readToTerminated(t *testing.T, c *websocket.Conn) {
	t.Helper()
	for !w.read(t, c) {
	}
}

// requireOrderedOnce requires strictly increasing seqs that include every seq
// in [0, n).
func (w *matrixWire) requireOrderedOnce(t *testing.T, n int64) {
	t.Helper()
	for i := 1; i < len(w.seqs); i++ {
		if w.seqs[i] <= w.seqs[i-1] {
			t.Fatalf("persisted events out of order or repeated at %d: %v", i, w.seqs)
		}
	}
	for s := int64(0); s < n; s++ {
		if !w.has(s) {
			t.Fatalf("persisted event %d never shipped: %v", s, w.seqs)
		}
	}
}

// endExternalRun flips an externally produced run to finished and closes its
// broker topic, the producer's end-of-events barrier: the merge checks the
// status at once and ends after its final drain.
func endExternalRun(t *testing.T, fx *matrixServer, id string) {
	t.Helper()
	mustWS(t, fx.native.UpdateRunStatus(t.Context(), id, store.RunStatusFinished, ""))
	fx.srv.runs.Broker().CloseRun(id)
}

// W5 (R7): alerts published while a replay is held in progress ship exactly
// once and move no cursor. No guard of the native wiring is specific to a
// replay in progress (one select serves replay and alerts): this witnesses
// exactly-once and the cursor while the replay is provably unfinished, not an
// alert's position among the replayed events.
func TestRunWSMatrixAlertWhileTheReplayIsInProgress(t *testing.T) {
	t.Cleanup(runview.SetMaxEventsPerPageForTest(1))
	const persisted = 40
	replayFromZero := json.RawMessage(`{"replay_history":true,"from_seq":0}`)

	// An unstamped alert (the browser sink's shape) and one stamped with the
	// origin, published while the replay is held; a fence after the release
	// proves the live loop judged both before the run ends. No alert manager:
	// the merge holds no registry lane, the branch where a stamped frame of
	// the origin ships rather than being dropped as a lane's carrier.
	t.Run("carriers", func(t *testing.T) {
		fx := newMatrixServer(t, matrixOptions{})
		ctx := t.Context()
		mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, store.LegacyValueFormat, store.RunStatusRunning, persisted))
		hold := fx.hook.holdNextEvents("r")
		c := dialRunWS(t, fx.hs, "r")
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, Payload: replayFromZero})
		snapshotOf(t, readEnvelope(t, c))
		hold.await(t)
		if n := fx.hook.relayedEvents("r"); n >= persisted {
			t.Fatalf("%d of %d persisted events relayed before the hold: the replay is not in progress", n, persisted)
		}
		origin, err := fx.srv.runs.CaptureRunOrigin(ctx, "r")
		mustWS(t, err)
		b := fx.srv.runs.Broker()
		b.Publish(matrixAlert(t, "r", "alert-unstamped", store.RunObservation{}))
		b.Publish(matrixAlert(t, "r", "alert-stamped", origin))
		hold.Release()
		b.Publish(matrixAlert(t, "r", "alert-fence", store.RunObservation{}))
		w := newMatrixWire()
		w.readUntil(t, c, func() bool { return w.alerts["alert-fence"] > 0 && w.has(persisted-1) })
		r, err := fx.native.LoadRun(ctx, "r")
		mustWS(t, err)
		write, err := store.BindRunValueFormat(ctx, r)
		mustWS(t, err)
		live, err := fx.native.AppendEvent(write, "r", store.Event{Type: store.EventNodeFinished, NodeID: "live-after-replay"})
		mustWS(t, err)
		b.Publish(*live)
		w.readUntil(t, c, func() bool { return w.has(live.Seq) })
		endExternalRun(t, fx, "r")
		w.readToTerminated(t, c)
		if live.Seq != persisted {
			t.Fatalf("fixture: the live event took seq %d, want %d", live.Seq, persisted)
		}
		w.requireOrderedOnce(t, persisted+1)
		if len(w.seqs) != persisted+1 {
			t.Fatalf("persisted events shipped = %v, want exactly 0..%d", w.seqs, persisted)
		}
		for _, name := range []string{"alert-unstamped", "alert-stamped", "alert-fence"} {
			if w.alerts[name] != 1 {
				t.Fatalf("alert frames %v: %s shipped %d times, want exactly once", w.alerts, name, w.alerts[name])
			}
		}
	})

	// A registered run: the manager publishes its registry frame, then the
	// stamped carrier on the broker. The run's end is the fence of both
	// lanes: every buffered frame ships or is deduped before terminated.
	t.Run("registered", func(t *testing.T) {
		fx := newMatrixServer(t, matrixOptions{alerts: true})
		if fx.srv.runs.AlertManager() == nil {
			t.Fatal("fixture: no alert manager")
		}
		ctx := t.Context()
		mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, store.LegacyValueFormat, store.RunStatusRunning, persisted))
		hold := fx.hook.holdNextEvents("r")
		c := dialRunWS(t, fx.hs, "r")
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, Payload: replayFromZero})
		snapshotOf(t, readEnvelope(t, c))
		hold.await(t)
		if n := fx.hook.relayedEvents("r"); n >= persisted {
			t.Fatalf("%d of %d persisted events relayed before the hold: the replay is not in progress", n, persisted)
		}
		// Subscribed after the merge's broker subscription: a frame on this
		// one was already fanned out to the merge's.
		observer := fx.srv.runs.Broker().Subscribe("r")
		defer observer.Cancel()
		origin, err := fx.srv.runs.CaptureRunOrigin(ctx, "r")
		mustWS(t, err)
		r, err := fx.native.LoadRun(ctx, "r")
		mustWS(t, err)
		write, err := store.BindRunValueFormat(ctx, r)
		mustWS(t, err)
		_, err = fx.native.AppendEvent(write, "r", store.Event{Type: store.EventBudgetWarning, RunID: "r", NodeID: "budget-node", Data: map[string]any{"dimension": "tokens"}})
		mustWS(t, err)
		awaitStampedCarrier(t, observer.C, "budget-node", origin)
		hold.Release()
		endExternalRun(t, fx, "r")
		w := newMatrixWire()
		w.readToTerminated(t, c)
		w.requireOrderedOnce(t, persisted)
		if w.alerts["budget-node"] != 1 {
			t.Fatalf("alert frames %v: the budget alert shipped %d times, want exactly once — the registry lane's frame, its stamped carrier deduped", w.alerts, w.alerts["budget-node"])
		}
	})
}

// awaitStampedCarrier reads a broker subscription until the alert of nodeID
// stamped with origin arrives: the manager's dispatch of it completed.
func awaitStampedCarrier(t *testing.T, ch <-chan *store.Event, nodeID string, origin store.RunObservation) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("the observer's broker subscription closed")
			}
			if ev.Type != store.EventAlert || ev.NodeID != nodeID {
				continue
			}
			if stamp, stamped := ev.EventObservation(); !stamped || stamp != origin {
				t.Fatalf("the manager's carrier is not stamped with the origin: %+v %t", stamp, stamped)
			}
			return
		case <-deadline:
			t.Fatalf("the manager never published the carrier of %s within 10s", nodeID)
		}
	}
}

// W6 (R7): with no alert manager the merge holds no registry lane and the
// binding filter is the only gate — production's case whenever a merge holds
// no lane; W9 runs the production configuration. An alert stamped with a
// successor's observation, then a fence stamped with the origin on the same
// broker subscription (FIFO): nothing of the successor reaches the socket
// before the fence.
func TestRunWSMatrixAnotherOriginsAlertNeverShips(t *testing.T) {
	fx := newMatrixServer(t, matrixOptions{})
	if fx.srv.runs.AlertManager() != nil {
		t.Fatal("fixture: an alert manager would register the run")
	}
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, store.LegacyValueFormat, store.RunStatusRunning, 1))
	c := dialRunWS(t, fx.hs, "r")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	snapshotOf(t, readEnvelope(t, c))
	awaitBrokerSubscriber(t, fx.srv, "r")
	origin, err := fx.srv.runs.CaptureRunOrigin(ctx, "r")
	mustWS(t, err)
	b := fx.srv.runs.Broker()
	b.Publish(matrixAlert(t, "r", "alert-of-the-successor", matrixSuccessorObservation(t, origin)))
	b.Publish(matrixAlert(t, "r", "fence-of-the-origin", origin))
	for fenced := false; !fenced; {
		for _, ev := range decodeEventEnvelope(t, readEnvelopeWithin(t, c, 5*time.Second, wsTypeEvent, wsTypeEventBatch)) {
			switch ev.NodeID {
			case "alert-of-the-successor":
				t.Fatal("an alert stamped with the successor's observation reached the socket bound to its predecessor")
			case "fence-of-the-origin":
				fenced = true
			}
		}
	}
}

// W7 (R9): a Service stop ends a bound stream typed — an event stream under
// Drain, a parked log stream under StopBackground and under Drain — and the
// socket stays open.
func TestRunWSMatrixDrainAndStopEndTheStreamsTyped(t *testing.T) {
	stop := map[string]func(*runview.Service, context.Context){
		"stop":  (*runview.Service).StopBackground,
		"drain": (*runview.Service).Drain,
	}
	t.Run("events/drain", func(t *testing.T) {
		fx := newMatrixServer(t, matrixOptions{})
		mustWS(t, seedWSOriginRun(t.Context(), fx.native, "r", matrixOrigin, store.LegacyValueFormat, store.RunStatusRunning, 1))
		c := dialRunWS(t, fx.hs, "r")
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
		snapshotOf(t, readEnvelope(t, c))
		awaitBrokerSubscriber(t, fx.srv, "r")
		stopMatrixService(fx, stop["drain"])
		expectMatrixSourceClosed(t, c, "event_stream_failed")
	})
	for _, kind := range []string{"stop", "drain"} {
		t.Run("logs/"+kind, func(t *testing.T) {
			fx := newMatrixServer(t, matrixOptions{})
			mustWS(t, seedWSOriginRun(t.Context(), fx.native, "r", matrixOrigin, store.LegacyValueFormat, store.RunStatusRunning, 0))
			const originLog = matrixOrigin + " log\n"
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", originLog)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
			readUntilAck(t, c, "l")
			if got := collectLogText(t, c, "", originLog, 5*time.Second); got != originLog {
				t.Fatalf("log before the stop = %q", got)
			}
			stopMatrixService(fx, stop[kind])
			expectMatrixSourceClosed(t, c, "log_stream_failed")
		})
	}
}

// stopMatrixService stops the run console within a bound: a stop that cannot
// join its streams returns at the deadline instead of hanging the test.
func stopMatrixService(fx *matrixServer, stop func(*runview.Service, context.Context)) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop(fx.srv.runs, ctx)
}

// expectMatrixSourceClosed requires the stream's end to be the code's error
// carrying runstream.ErrSourceClosed — never a clean completion — then a pong:
// the socket stays open.
func expectMatrixSourceClosed(t *testing.T, c *websocket.Conn, code string) {
	t.Helper()
	env := readEnvelopeWithin(t, c, 10*time.Second, wsTypeError, wsTypeTerminated, wsTypeLogTerminated)
	if env.Type != wsTypeError {
		t.Fatalf("the stream ended with %s, want the %s error: a Service stop is no clean completion", env.Type, code)
	}
	if p := errorCodeOf(t, env); p.Code != code || !strings.Contains(p.Message, runstream.ErrSourceClosed.Error()) {
		t.Fatalf("error = %+v, want %s carrying %q", p, code, runstream.ErrSourceClosed)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypePing, AckID: "alive"})
	if pong := readEnvelope(t, c, wsTypePong); pong.AckID != "alive" {
		t.Fatalf("pong = %+v: the socket did not stay open", pong)
	}
}

// W9 (R7, the production configuration): an alert manager is wired, as on
// every production server. Socket SA streams the origin's events, its log and
// a registered alert; the origin moves aside and an external successor takes
// its ID, which socket SB opens and streams while the successor appends
// budget alerts and log bytes. SA receives nothing of the successor and ends
// with a typed refusal and the scope_lost close; SB receives the successor's
// alert and nothing of the origin. This row qualifies the production
// configuration end to end; it is not the witness of its guards: the
// reservations under the upgrade's binding redden it only on some
// interleavings (the log parent unbound about two runs in three, the event
// parent rarely), and W1-W4 witness them deterministically.
func TestRunWSMatrixTwoSocketsAcrossAReplacementWithAlerts(t *testing.T) {
	fx := newMatrixServer(t, matrixOptions{alerts: true})
	if fx.srv.runs.AlertManager() == nil {
		t.Fatal("fixture: no alert manager")
	}
	ctx := t.Context()
	storeDir := fx.srv.cfg.StoreDir
	const originLog = matrixOrigin + " log\n"
	originAlert := "budget-" + matrixOrigin
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixOrigin, store.LegacyValueFormat, store.RunStatusRunning, 1))
	writeRunLog(t, storeDir, "r", originLog)
	sa := dialRunWS(t, fx.hs, "r")
	writeJSONMessage(t, sa, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
	snapshotOf(t, readEnvelopeWithin(t, sa, 10*time.Second, wsTypeSnapshot, wsTypeError))
	awaitBrokerSubscriber(t, fx.srv, "r")
	writeJSONMessage(t, sa, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
	appendMatrixBudgetWarning(t, fx, "r", originAlert)
	sawAlert, logText := false, ""
	for !sawAlert || logText != originLog {
		env := readEnvelopeWithin(t, sa, 15*time.Second)
		switch env.Type {
		case wsTypeEvent, wsTypeEventBatch:
			for _, ev := range decodeEventEnvelope(t, env) {
				if ev.Type == store.EventAlert && ev.NodeID == originAlert {
					sawAlert = true
				}
			}
		case wsTypeLogChunk:
			logText += decodeLogChunk(t, env).Text
		case wsTypeError, wsTypeTerminated, wsTypeLogTerminated:
			t.Fatalf("SA ended before the replacement: %s %s", env.Type, env.Payload)
		}
	}

	// The origin moves aside, as an external writer would, and a running
	// successor takes the ID.
	mustWS(t, os.Rename(filepath.Join(storeDir, "runs", "r"), filepath.Join(storeDir, "aside-r")))
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", matrixSuccessor, store.LegacyValueFormat, store.RunStatusRunning, 2))
	successorLog := writeRunLog(t, storeDir, "r", matrixSuccessor+" log\n")
	sb := dialRunWS(t, fx.hs, "r")
	writeJSONMessage(t, sb, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s", Payload: json.RawMessage(`{"replay_history":true,"from_seq":0}`)})
	if snap := snapshotOf(t, readEnvelopeWithin(t, sb, 10*time.Second, wsTypeSnapshot, wsTypeError)); snap.Run.WorkflowName != matrixSuccessor {
		t.Fatalf("SB snapshot names %q", snap.Run.WorkflowName)
	}
	writeJSONMessage(t, sb, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
	// The successor's budget alerts are published as the in-process order
	// would, and its log grows; the pacing spreads them over the detection.
	for i := 0; i < 3; i++ {
		ev := appendMatrixBudgetWarning(t, fx, "r", fmt.Sprintf("budget-%s-%d", matrixSuccessor, i))
		fx.srv.runs.Broker().Publish(*ev)
		appendRunLog(t, successorLog, fmt.Sprintf("%s more %d\n", matrixSuccessor, i))
		time.Sleep(300 * time.Millisecond)
	}

	// SA: nothing of the successor, then a typed refusal and the scope_lost
	// close; no clean end.
	_ = sa.SetReadDeadline(time.Now().Add(30 * time.Second))
	var refusals []string
	for {
		kind, raw, err := sa.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("SA read until its scope_lost close: %v (refusals %v)", err, refusals)
			}
			if ce.Code != websocket.CloseNormalClosure || !strings.HasPrefix(ce.Text, wsCloseScopeLost+": ") {
				t.Fatalf("SA close = %d %q, want %d %s", ce.Code, ce.Text, websocket.CloseNormalClosure, wsCloseScopeLost)
			}
			break
		}
		if kind != websocket.TextMessage {
			continue
		}
		if strings.Contains(string(raw), matrixSuccessor) {
			t.Fatalf("a frame of the successor reached the origin's socket: %s", raw)
		}
		var env runWSEnvelope
		mustWS(t, json.Unmarshal(raw, &env))
		switch env.Type {
		case wsTypeSnapshot, wsTypeTerminated, wsTypeLogTerminated:
			t.Fatalf("SA got %s after the replacement, want a typed refusal: %s", env.Type, raw)
		case wsTypeError:
			refusals = append(refusals, errorCodeOf(t, env).Code)
		}
	}
	if len(refusals) == 0 {
		t.Fatal("SA closed scope_lost without a typed refusal")
	}
	for _, code := range refusals {
		if code != "event_stream_failed" && code != "log_stream_failed" {
			t.Fatalf("SA refusals = %v, want event_stream_failed or log_stream_failed", refusals)
		}
	}

	// SB: the successor's alert, nothing of the origin.
	_ = sb.SetReadDeadline(time.Now().Add(20 * time.Second))
	for gotAlert := false; !gotAlert; {
		_, raw, err := sb.ReadMessage()
		if err != nil {
			t.Fatalf("SB never received the successor's alert: %v", err)
		}
		if strings.Contains(string(raw), matrixOrigin) {
			t.Fatalf("a frame of the origin reached the successor's socket: %s", raw)
		}
		var env runWSEnvelope
		mustWS(t, json.Unmarshal(raw, &env))
		switch env.Type {
		case wsTypeError, wsTypeTerminated, wsTypeLogTerminated:
			t.Fatalf("SB ended: %s", raw)
		case wsTypeEvent, wsTypeEventBatch:
			for _, ev := range decodeEventEnvelope(t, env) {
				if ev.Type == store.EventAlert && strings.HasPrefix(ev.NodeID, "budget-"+matrixSuccessor) {
					gotAlert = true
				}
			}
		}
	}
}

// appendMatrixBudgetWarning appends a budget warning to id's events under the
// run's own format: the alert manager turns it into an alert of node.
func appendMatrixBudgetWarning(t *testing.T, fx *matrixServer, id, node string) *store.Event {
	t.Helper()
	r, err := fx.native.LoadRun(t.Context(), id)
	mustWS(t, err)
	write, err := store.BindRunValueFormat(t.Context(), r)
	mustWS(t, err)
	ev, err := fx.native.AppendEvent(write, id, store.Event{Type: store.EventBudgetWarning, RunID: id, NodeID: node, Data: map[string]any{"dimension": "tokens"}})
	mustWS(t, err)
	return ev
}
