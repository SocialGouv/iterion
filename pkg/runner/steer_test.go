package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// steerRunnerFixture wires a Runner with the ack transport stubbed and
// an "engine" goroutine that acks overrides with canned results.
type steerRunnerFixture struct {
	r       *Runner
	acks    []runview.SteerReply
	ackByID map[string]runview.SteerReply
}

func newSteerRunner(t *testing.T) *steerRunnerFixture {
	t.Helper()
	f := &steerRunnerFixture{ackByID: map[string]runview.SteerReply{}}
	f.r = &Runner{cfg: Config{Logger: iterlog.Nop(), RunnerID: "runner-test"}}
	f.r.steerAckTimeout = 500 * time.Millisecond
	f.r.steerAckFn = func(_, _ string, body []byte) error {
		var reply runview.SteerReply
		if err := json.Unmarshal(body, &reply); err != nil {
			t.Fatalf("unmarshal reply: %v", err)
		}
		f.acks = append(f.acks, reply)
		f.ackByID[reply.CommandID] = reply
		return nil
	}
	return f
}

func steerBody(t *testing.T, cmd runview.SteerCommand) []byte {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandleSteerDelivery_TranslatesAndReplies(t *testing.T) {
	f := newSteerRunner(t)
	ch := f.r.registerSteerChannel(context.Background(), "r1")
	defer f.r.unregisterSteerChannel("r1")

	// Engine stand-in: ack the bump with applied/effective values.
	go func() {
		msg := <-ch
		msg.Ack(runtime.OverrideResult{
			Applied:   map[string]any{"loop": "retry", "delta": 2, "extra": 2},
			Effective: map[string]any{"effective_max": 5, "current": 3},
		})
	}()

	f.r.handleSteerDelivery("r1", steerBody(t, runview.SteerCommand{
		CommandID: "cmd-1", Kind: runview.SteerBumpLoop, LoopName: "retry", Delta: 2,
	}), "cmd-1")

	if len(f.acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(f.acks))
	}
	reply := f.acks[0]
	if reply.Err != nil || reply.RunnerID != "runner-test" || reply.Effective["effective_max"] != float64(5) {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestHandleSteerDelivery_TypedErrorRoundTrip(t *testing.T) {
	f := newSteerRunner(t)
	ch := f.r.registerSteerChannel(context.Background(), "r2")
	defer f.r.unregisterSteerChannel("r2")

	go func() {
		msg := <-ch
		msg.Ack(runtime.OverrideResult{Err: &runtime.UnknownLoopError{Loop: "nope", Available: []string{"retry"}}})
	}()
	f.r.handleSteerDelivery("r2", steerBody(t, runview.SteerCommand{
		CommandID: "cmd-2", Kind: runview.SteerBumpLoop, LoopName: "nope", Delta: 1,
	}), "cmd-2")

	reply := f.acks[0]
	if reply.Err == nil || reply.Err.Code != "unknown_loop" {
		t.Fatalf("reply = %+v, want unknown_loop", reply)
	}
	if avail, ok := reply.Err.Details["available_loops"].([]any); !ok || len(avail) != 1 {
		t.Fatalf("details = %+v, want available_loops", reply.Err.Details)
	}
}

func TestHandleSteerDelivery_NotActiveRun(t *testing.T) {
	f := newSteerRunner(t)
	f.r.handleSteerDelivery("ghost", steerBody(t, runview.SteerCommand{
		CommandID: "cmd-3", Kind: runview.SteerBumpLoop, LoopName: "l", Delta: 1,
	}), "cmd-3")
	if f.acks[0].Err == nil || f.acks[0].Err.Code != "not_active" {
		t.Fatalf("reply = %+v, want not_active", f.acks[0])
	}
}

func TestHandleSteerDelivery_EngineBusyIsStalled(t *testing.T) {
	f := newSteerRunner(t)
	f.r.registerSteerChannel(context.Background(), "r3")
	defer f.r.unregisterSteerChannel("r3")

	// Nothing drains the channel → ack timeout → engine_stalled.
	f.r.handleSteerDelivery("r3", steerBody(t, runview.SteerCommand{
		CommandID: "cmd-4", Kind: runview.SteerBumpLoop, LoopName: "l", Delta: 1,
	}), "cmd-4")
	if f.acks[0].Err == nil || f.acks[0].Err.Code != "engine_stalled" {
		t.Fatalf("reply = %+v, want engine_stalled", f.acks[0])
	}
}

func TestHandleSteerDelivery_DedupReplaysCachedReply(t *testing.T) {
	f := newSteerRunner(t)
	ch := f.r.registerSteerChannel(context.Background(), "r4")
	defer f.r.unregisterSteerChannel("r4")

	applies := 0
	go func() {
		for msg := range ch {
			applies++
			msg.Ack(runtime.OverrideResult{Applied: map[string]any{"extra": applies}})
		}
	}()

	body := steerBody(t, runview.SteerCommand{CommandID: "dup-1", Kind: runview.SteerBumpLoop, LoopName: "l", Delta: 1})
	f.r.handleSteerDelivery("r4", body, "dup-1")
	f.r.handleSteerDelivery("r4", body, "dup-1") // transport retry

	if len(f.acks) != 2 {
		t.Fatalf("acks = %d, want 2 (original + replay)", len(f.acks))
	}
	if applies != 1 {
		t.Fatalf("engine applied %d times, want 1 (dedup)", applies)
	}
	if f.acks[0].Applied["extra"] != f.acks[1].Applied["extra"] {
		t.Fatalf("replayed reply differs: %+v vs %+v", f.acks[0], f.acks[1])
	}
}

func TestHandleSteerDelivery_RaiseBudgetRequiresBudget(t *testing.T) {
	f := newSteerRunner(t)
	f.r.registerSteerChannel(context.Background(), "r5")
	defer f.r.unregisterSteerChannel("r5")

	f.r.handleSteerDelivery("r5", steerBody(t, runview.SteerCommand{
		CommandID: "cmd-6", Kind: runview.SteerRaiseBudget,
	}), "cmd-6")
	if f.acks[0].Err == nil || f.acks[0].Err.Code != "invalid" {
		t.Fatalf("reply = %+v, want invalid (nil budget)", f.acks[0])
	}

	// With a budget object it reaches the engine.
	ch := f.r.steerChannelFor("r5")
	go func() {
		msg := <-ch
		msg.Ack(runtime.OverrideResult{Noop: true, NoopReason: "caps not exceeded"})
	}()
	f.r.handleSteerDelivery("r5", steerBody(t, runview.SteerCommand{
		CommandID: "cmd-7", Kind: runview.SteerRaiseBudget, Budget: &ir.BudgetOverrides{MaxTokens: 10},
	}), "cmd-7")
	last := f.acks[len(f.acks)-1]
	if last.Err != nil || !last.Noop {
		t.Fatalf("reply = %+v, want noop", last)
	}
}
