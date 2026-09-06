package runner

import (
	"context"
	"errors"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A RunMessage whose IR this runner cannot decode or compile is a typed
// error, not a generic one — the delivery loop must be able to tell it
// from a transient.
func TestLoadWorkflow_UnloadableIRIsTyped(t *testing.T) {
	for _, tc := range []struct {
		name string
		ir   string
	}{
		{"undecodable bytes", "not json"},
		{"decodable, uncompilable", "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWorkflow(context.Background(), &queue.RunMessage{RunID: "r", IRCompiled: []byte(tc.ir)}, nil)
			if !errors.Is(err, ErrIRUnloadable) {
				t.Fatalf("err = %v, want ErrIRUnloadable", err)
			}
		})
	}
}

// failUnloadableIR writes on the run what a mute runner never wrote: the
// resumable failure with its code, and a run_failed event that names the
// runner version beside the workflow hash.
func TestFailUnloadableIR_WritesTheVerdictOnTheRun(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-skew", "wf", nil); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	msg := &queue.RunMessage{RunID: "run-skew", WorkflowHash: "e7e2f6e6"}
	r.failUnloadableIR(ctx, msg, errors.New("runner: the IR does not load on this runner: compile IR: 3 diagnostic(s)"))

	run, err := st.LoadRun(ctx, "run-skew")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %q, want failed_resumable (a resume after the fleet is aligned re-compiles on the server)", run.Status)
	}
	if run.FailureCode != "IR_UNLOADABLE" {
		t.Fatalf("failure code = %q, want IR_UNLOADABLE", run.FailureCode)
	}
	evs, _ := st.LoadEvents(ctx, "run-skew")
	var found bool
	for _, ev := range evs {
		if ev.Type == store.EventRunFailed && ev.Data["code"] == "IR_UNLOADABLE" {
			found = true
			if ev.Data["runner_version"] == nil || ev.Data["workflow_hash"] != "e7e2f6e6" {
				t.Fatalf("run_failed must carry the runner version and the workflow hash: %v", ev.Data)
			}
		}
	}
	if !found {
		t.Fatal("no run_failed {code: IR_UNLOADABLE} event: the skew is not readable from the run")
	}
}

// The wiring: executeRun, handed a message whose IR does not load, writes
// the verdict on the run before returning the typed error — the delivery
// loop then acks it and nothing is left to a redelivery.
func TestExecuteRun_UnloadableIRWritesTheVerdictBeforeReturning(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-skew-2", "wf", nil); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	msg := &queue.RunMessage{RunID: "run-skew-2", WorkflowHash: "e7e2f6e6", IRCompiled: []byte("not json")}
	execErr := r.executeRun(ctx, msg, nil)
	if !errors.Is(execErr, ErrIRUnloadable) {
		t.Fatalf("executeRun err = %v, want ErrIRUnloadable", execErr)
	}
	if got := classifyExecResult(execErr, msg.RunID); got.action != actionAck || got.finalStatus != "ir_unloadable" {
		t.Fatalf("classified as %s/%v, want ir_unloadable/ack", got.finalStatus, got.action)
	}
	run, _ := st.LoadRun(ctx, "run-skew-2")
	if run.Status != store.RunStatusFailedResumable || run.FailureCode != "IR_UNLOADABLE" {
		t.Fatalf("run = %s/%s, want failed_resumable/IR_UNLOADABLE written before the return", run.Status, run.FailureCode)
	}
}
