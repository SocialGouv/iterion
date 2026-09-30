package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestSpendScratchConsent_isSpentByTheClaimOfItsPublication: a resume
// message's consent to the scratch's loss is applied only while the run is
// still queued for that message. Redelivered after its claim — the run
// running or parked again — adopted, or behind a later attempt's queued
// flip, the message no longer carries it; so does a message whose
// publication time cannot be read.
func TestSpendScratchConsent_isSpentByTheClaimOfItsPublication(t *testing.T) {
	published := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		status    store.RunStatus
		queuedAt  time.Time
		published string
		kept      bool
	}{
		{"queued for this message", store.RunStatusQueued, published.Add(-time.Second), published.Format(time.RFC3339Nano), true},
		{"claimed, then parked again", store.RunStatusFailedResumable, published.Add(-time.Second), published.Format(time.RFC3339Nano), false},
		{"claimed, left running", store.RunStatusRunning, published.Add(-time.Second), published.Format(time.RFC3339Nano), false},
		{"queued for a later attempt", store.RunStatusQueued, published.Add(time.Minute), published.Format(time.RFC3339Nano), false},
		{"a publication time that cannot be read", store.RunStatusQueued, published.Add(-time.Second), "yesterday", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			queuedAt := tc.queuedAt
			if err := st.SaveRun(ctx, &store.Run{ID: "run-consent", TenantID: "team-1", OwnerID: "u1", Status: tc.status, QueuedAt: &queuedAt}); err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.Nop()}}
			msg := &queue.RunMessage{RunID: "run-consent", TenantID: "team-1", OwnerID: "u1", PublishedAtRFC: tc.published,
				Resume: &queue.ResumeSpec{Force: true, AcceptScratchLoss: true}}
			r.spendScratchConsent(context.Background(), msg, iterlog.Nop())
			if msg.Resume.AcceptScratchLoss != tc.kept || !msg.Resume.Force {
				t.Fatalf("after the claim check: consent %v (want %v), force %v (want it untouched)", msg.Resume.AcceptScratchLoss, tc.kept, msg.Resume.Force)
			}
		})
	}
}

// TestExecuteRun_givesTheEngineTheScratchsConsent: the consent a resume
// message carries reaches the engine that executes it — a run whose last
// teardown could not bank its scratch resumes with it, and is refused with
// force alone.
func TestExecuteRun_givesTheEngineTheScratchsConsent(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(fmt.Sprintf("accept=%v", accept), func(t *testing.T) {
			t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
			ctx := store.WithIdentity(context.Background(), "team-1", "u1")
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const runID = "run-consent-wire"
			if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateRunStatus(ctx, runID, store.RunStatusFailedResumable, "boom"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
				"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
			}}); err != nil {
				t.Fatal(err)
			}
			pr := parser.Parse("main.bot", "workflow main:\n  entry: done\n")
			body, err := ast.MarshalFile(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.Nop()}}
			msg := &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
				Resume: &queue.ResumeSpec{Force: true, AcceptScratchLoss: accept}}
			execErr := r.executeRun(ctx, msg, nil)
			var rt *runtime.RuntimeError
			refused := errors.As(execErr, &rt) && rt.Code == runtime.ErrCodeScratchNotPortable
			if refused == accept {
				t.Fatalf("executeRun with consent %v: %v, want refused = %v", accept, execErr, !accept)
			}
		})
	}
}
