package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// genericForceAdvice is the remedy the runner writes for a failure that
// names none of its own.
const genericForceAdvice = "iterion resume --force"

// scratchRefusedDelivery is a cloud resume of a run whose last teardown
// recorded the scratch as scratchRecord, published after the publisher's
// queued flip and executed through processOne's tail: the engine, the
// release, and the record of the delivery's end.
func scratchRefusedDelivery(t *testing.T, scratchRecord map[string]any, runHash, msgHash string, force bool) (store.RunStore, string) {
	t.Helper()
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-scratch-remedy"
	if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
		t.Fatal(err)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = runHash
	run.Checkpoint = &store.Checkpoint{NodeID: "done"}
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "boom"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: scratchRecord}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{store.RunStatusFailedResumable}); err != nil || !ok {
		t.Fatalf("the publisher's queued flip: ok=%v err=%v", ok, err)
	}
	pr := parser.Parse("main.bot", "workflow main:\n  entry: done\n")
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
		WorkflowHash:   msgHash,
		PublishedAtRFC: time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
		Resume:         &queue.ResumeSpec{Force: force, PriorStatus: store.RunStatusFailedResumable}}
	execErr := r.executeRun(ctx, msg, nil, nil)
	if execErr == nil {
		t.Fatal("the engine resumed a run whose scratch did not travel")
	}
	outcome := classifyExecResult(execErr, runID)
	var released store.RunStatus
	if !isNakAction(outcome.action) {
		released = r.releaseRefusedResume(msg, execErr, iterlog.Nop())
	}
	r.recordDeliveryEnd(msg, execErr, outcome.finalStatus, released)
	return st, runID
}

func lastEventOf(t *testing.T, st store.RunStore, runID string, typ store.EventType) *store.Event {
	t.Helper()
	evs, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == typ {
			return evs[i]
		}
	}
	return nil
}

// TestRecordDeliveryEnd_aScratchRefusalNamesItsOwnConsent: a cloud resume the
// engine refused over the scratch — before its claim (a scratch the teardown
// could not bank) or after it (a bank the restore cannot find, here a resume
// without a sandbox) — says, everywhere an operator reads it, the consent
// that clears it: the run's error, run_failed and run_retry_skipped name
// --accept-scratch-loss, and run_retry_skipped never sends the operator to
// --force, which the next resume would meet the same refusal with.
func TestRecordDeliveryEnd_aScratchRefusalNamesItsOwnConsent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		record       map[string]any
		wantReleased bool
	}{
		{"before the claim: a scratch its teardown could not bank", map[string]any{"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap"}, true},
		{"after the claim: a bank the restore cannot find", map[string]any{"banked": true, "bytes": 42}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, runID := scratchRefusedDelivery(t, tc.record, "", "", true)
			doc, err := st.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Status != store.RunStatusFailedResumable || doc.FailureCode != store.FailureScratchNotPortable {
				t.Fatalf("doc = %s/%s, want failed_resumable/SCRATCH_NOT_PORTABLE", doc.Status, doc.FailureCode)
			}
			if !strings.Contains(doc.Error, "--accept-scratch-loss") {
				t.Errorf("the run's error does not name the consent that clears it: %q", doc.Error)
			}
			skipped := lastEventOf(t, st, runID, store.EventRunRetrySkipped)
			if skipped == nil {
				t.Fatal("no run_retry_skipped")
			}
			hint, _ := skipped.Data["hint"].(string)
			if !strings.Contains(hint, "--accept-scratch-loss") || strings.Contains(hint, "--force") {
				t.Errorf("run_retry_skipped hint %q: want the scratch's own consent, never --force", hint)
			}
			if _, released := skipped.Data["status"]; released != tc.wantReleased {
				t.Errorf("run_retry_skipped status present = %v, want %v", released, tc.wantReleased)
			}
			if tc.wantReleased {
				return
			}
			failed := lastEventOf(t, st, runID, store.EventRunFailed)
			if failed == nil {
				t.Fatal("the park after the claim wrote no run_failed")
			}
			if hint, _ := failed.Data["hint"].(string); !strings.Contains(hint, "--accept-scratch-loss") {
				t.Errorf("run_failed hint %q does not name the consent", hint)
			}
		})
	}
}

// TestRecordDeliveryEnd_aScratchRefusalOverAnEditedSourceNamesBothConsents: a
// refusal that names a change --force accepts as well says so on the
// timeline too, in the hint and as also_needs_force.
func TestRecordDeliveryEnd_aScratchRefusalOverAnEditedSourceNamesBothConsents(t *testing.T) {
	st, runID := scratchRefusedDelivery(t, map[string]any{"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap"}, "hash-at-launch", "hash-edited", false)
	skipped := lastEventOf(t, st, runID, store.EventRunRetrySkipped)
	if skipped == nil {
		t.Fatal("no run_retry_skipped")
	}
	hint, _ := skipped.Data["hint"].(string)
	if !strings.Contains(hint, "--accept-scratch-loss") || !strings.Contains(hint, "--force") {
		t.Errorf("hint %q: want both consents named", hint)
	}
	if skipped.Data["also_needs_force"] != true {
		t.Errorf("run_retry_skipped also_needs_force = %v, want true", skipped.Data["also_needs_force"])
	}
}

// TestRecordRetrySkipped_aScratchCodeReadBackNeverGetsTheForceAdvice: a
// redelivery dropped on the document's code has no error to read the remedy
// from; the scratch's code names its own consent all the same, and every
// other code keeps the generic advice.
func TestRecordRetrySkipped_aScratchCodeReadBackNeverGetsTheForceAdvice(t *testing.T) {
	for _, tc := range []struct {
		code      store.FailureCode
		want, not string
	}{
		{store.FailureScratchNotPortable, "--accept-scratch-loss", "--force"},
		{store.FailureExpressionFailed, genericForceAdvice, "--accept-scratch-loss"},
	} {
		t.Run(string(tc.code), func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			const id = "run-code-read-back"
			if err := st.SaveRun(ctx, &store.Run{ID: id, TenantID: "team-1", OwnerID: "u1", Status: store.RunStatusFailedResumable}); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			r.recordRetrySkipped(&queue.RunMessage{RunID: id, TenantID: "team-1", OwnerID: "u1"}, tc.code, "the document's words", nil, "")
			skipped := lastEventOf(t, st, id, store.EventRunRetrySkipped)
			if skipped == nil {
				t.Fatal("no run_retry_skipped")
			}
			hint, _ := skipped.Data["hint"].(string)
			if !strings.Contains(hint, tc.want) || strings.Contains(hint, tc.not) {
				t.Errorf("hint %q: want %q, never %q", hint, tc.want, tc.not)
			}
		})
	}
}
