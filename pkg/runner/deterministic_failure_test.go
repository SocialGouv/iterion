package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Measured 2026-09-06 on run 01a07804 (the Billy 1.6.0 dogfood): a
// `compute` node whose expression the runner's evaluator could not satisfy
// failed, the delivery naked, JetStream redelivered, dispositionForStatus
// synthesised a resume — seven times in ten minutes, each one a fresh pod,
// a fresh clone and a fresh sandbox, all reaching the identical verdict. A
// compute node runs no LLM and no shell and its inputs come from a
// checkpoint that does not move: nothing between two attempts could differ.
func TestClassifyExecResult_DeterministicNodeFailureAcks(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"compute expression", &runtime.RuntimeError{
			Code:    store.FailureExpressionFailed,
			NodeID:  "delivery_reserve",
			Message: `compute "delivery_reserve": field "minutes" expression "max(a, b, c)": expr: max() takes 2 arguments, got 3`,
		}},
		{"permanent tool failure", &runtime.RuntimeError{
			Code: store.FailureToolFailedPermanent, NodeID: "verify_run", Message: "exit status 2",
		}},
		// The conversation rides the checkpoint and the in-node recipe
		// already compacted twice: no attempt after this one is smaller.
		{"context length exceeded", &runtime.RuntimeError{
			Code: store.FailureContextLengthExceeded, NodeID: "campaign",
			Message: "compaction did not reduce context enough to fit the model window",
		}},
		{"wrapped expression failure", fmt.Errorf("engine: %w", &runtime.RuntimeError{
			Code: store.FailureExpressionFailed, Message: "boom",
		})},
		// Measured 2026-09-07 on run 01a07db7: four pods for a schema the
		// serving backend could not read. The declaration rides the IR.
		{"a declared schema the backend cannot read", &runtime.RuntimeError{
			Code: store.FailureSchemaUnusable, NodeID: "voter_v2",
			Message: "parse ExplicitSchema: json: cannot unmarshal array into Go struct field InputSchema.properties.verdicts.type of type string",
		}},
		// Measured 2026-09-07 on run 01a07da6: eight pods in 77 seconds
		// for a model the ChatGPT backend does not serve to this image.
		{"a model the provider will not serve", &runtime.RuntimeError{
			Code: store.FailureModelUnavailable, NodeID: "m_astra",
			Message: `backend "claw" failed: openai: API error 400: {"detail":"The 'gpt-6-astra' model requires a newer version of Codex."}`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := classifyExecResult(c.err, "run-1")
			if out.action != actionAck {
				t.Errorf("action = %v, want actionAck — a NAK redelivers a failure that can only repeat", out.action)
			}
			if out.finalStatus != "deterministic_failure" {
				t.Errorf("finalStatus = %q, want deterministic_failure", out.finalStatus)
			}
		})
	}

	// The classes an automatic resume CAN cure keep naking — this carve-out
	// must not swallow the redelivery path the fleet runs on.
	for _, c := range []struct {
		name string
		err  error
	}{
		{"transient backend fault", &runtime.RuntimeError{Code: store.FailureExecutionFailed, Message: "upstream 502"}},
		{"unclassified error", errors.New("boom")},
		// An agent's output that missed its schema is NOT deterministic:
		// the next sample may conform, so the redelivery keeps its job.
		{"schema validation on an agent output", &runtime.RuntimeError{
			Code: store.FailureSchemaValidation, NodeID: "plan", Message: "output does not match schema",
		}},
		// Nor is a refused credential: every claim re-materialises the
		// sealed OAuth blob and refreshes it, so the EFFECTIVE token on the
		// next attempt can differ from the one that was refused.
		{"auth failed", &runtime.RuntimeError{Code: store.FailureAuthFailed, Message: "401 invalid api key"}},
	} {
		t.Run(c.name+" still naks", func(t *testing.T) {
			if out := classifyExecResult(c.err, "run-1"); out.action != actionNak {
				t.Errorf("action = %v, want actionNak", out.action)
			}
		})
	}
}

// The belt: a delivery that lands on a run already parked with a
// deterministic code must not synthesise a resume either. This is the arm
// that actually produced the seven attempts — classifyExecResult naked, and
// THIS turned every redelivery back into a run.
func TestDispositionForStatus_DeterministicCodeIsNotAutoResumed(t *testing.T) {
	msg := &queue.RunMessage{RunID: "run-1"}
	run := &store.Run{
		ID:          "run-1",
		Status:      store.RunStatusFailedResumable,
		FailureCode: store.FailureExpressionFailed,
	}
	out := dispositionForStatus(msg, run)
	if out.proceed {
		t.Error("proceed = true — the runner re-ran a deterministic failure against an unchanged checkpoint")
	}
	if msg.Resume != nil {
		t.Error("a resume was synthesised for a deterministic engine code")
	}
	if out.action != actionAck {
		t.Errorf("action = %v, want actionAck (the run stays failed_resumable for an operator)", out.action)
	}
}

// The timeline has to say why a run stopped coming back: a failed_resumable
// row with no further attempt is otherwise indistinguishable from one whose
// redelivery is still in flight.
func TestRecordRetrySkipped_PutsTheDeterministicVerdictOnTheTimeline(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const id = "run-deterministic"
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	if err := st.SaveRun(ctx, &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusFailedResumable}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}

	r.recordRetrySkipped(msg, store.FailureExpressionFailed,
		`compute "delivery_reserve": expr: max() takes 2 arguments, got 3`)

	events, err := st.LoadEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var ev *store.Event
	for _, e := range events {
		if e.Type == store.EventRunRetrySkipped {
			ev = e
		}
	}
	if ev == nil {
		t.Fatal("no run_retry_skipped on the timeline — the run just stops, with nothing saying why")
	}
	if got, _ := ev.Data["reason"].(string); got != "deterministic" {
		t.Errorf("reason = %q, want deterministic", got)
	}
	if got, _ := ev.Data["code"].(string); got != string(store.FailureExpressionFailed) {
		t.Errorf("code = %q, want %s", got, store.FailureExpressionFailed)
	}
	if got, _ := ev.Data["error"].(string); got == "" {
		t.Error("error is empty — the operator cannot tell which expression refused")
	}
}
