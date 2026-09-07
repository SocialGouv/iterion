package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Measured 2026-09-05 (cloud runner v3.102.0, kubernetes sandbox): a run
// whose sandbox pod never became Ready ended
//
//	status = failed · final_error = None · events = the three sandbox
//	markers, and nothing else
//
// No run_failed, no code, no reason. A headless router that triages
// terminals by the tree cannot classify that at all, so an infrastructure
// timeout lands in a human queue — and an operator reading the API sees a
// terminal failure with nothing to read.
//
// Every setup phase between "the run was claimed" and "the first node" goes
// through markFailedBestEffort, which wrote the document and emitted
// nothing. This drives that single chokepoint on each phase it serves.
func TestMarkFailedBestEffort_EmitsRunFailedWithACode(t *testing.T) {
	cases := []struct {
		name       string
		phase      string
		cause      error
		wantStatus store.RunStatus
		wantCode   store.FailureCode
		wantErr    string
	}{
		{
			// #697's own shape once the driver's bound is what fires.
			name:  "sandbox start phase timeout",
			phase: "sandbox start",
			cause: fmt.Errorf("runtime: sandbox: %w", errors.Join(sandbox.ErrPhaseTimeout,
				errors.New("kubectl wait pod/iterion-sbx-x: timed out after 180s"))),
			wantStatus: store.RunStatusFailedResumable,
			wantCode:   store.FailureSandboxSetupTimeout,
			wantErr:    "timed out after 180s",
		},
		{
			// A sandbox that fails for a reason no sentinel names — a bad
			// image reference, an invalid spec — is the terminal `failed`
			// the ticket was filed on.
			name:       "sandbox start driver error",
			phase:      "sandbox start",
			cause:      errors.New("runtime: sandbox: kubernetes: create pod: admission webhook denied the request"),
			wantStatus: store.RunStatusFailed,
			wantErr:    "admission webhook denied",
		},
		{
			// The other seven phases share the chokepoint; a var the launch
			// could not validate is the earliest of them.
			name:       "var validation",
			phase:      "var validation",
			cause:      &RuntimeError{Code: store.FailureExecutionFailed, Message: "var \"repo\" is required"},
			wantStatus: store.RunStatusFailed,
			wantCode:   store.FailureExecutionFailed,
			wantErr:    "is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			runID := "run-setup-" + strings.ReplaceAll(tc.name, " ", "-")
			if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			eng := New(devboxTestWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop()))

			eng.markFailedBestEffort(ctx, runID, tc.phase, tc.cause)

			r, err := s.LoadRun(ctx, runID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if r.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", r.Status, tc.wantStatus)
			}
			if !strings.Contains(r.Error, tc.wantErr) {
				t.Errorf("run.Error = %q, want it to carry the driver's message %q", r.Error, tc.wantErr)
			}
			if tc.wantCode != "" && r.FailureCode != tc.wantCode {
				t.Errorf("failure_code = %q, want %q", r.FailureCode, tc.wantCode)
			}

			events, err := s.LoadEvents(ctx, runID)
			if err != nil {
				t.Fatalf("load events: %v", err)
			}
			var failed *store.Event
			for _, e := range events {
				if e.Type == store.EventRunFailed {
					failed = e
				}
			}
			if failed == nil {
				t.Fatalf("no run_failed event — the API shows a terminal %s with nothing to read (events: %v)",
					tc.wantStatus, eventTypes(events))
			}
			if got, _ := failed.Data["error"].(string); !strings.Contains(got, tc.wantErr) {
				t.Errorf("run_failed.error = %q, want the driver's message %q", got, tc.wantErr)
			}
			if got, _ := failed.Data["phase"].(string); got != tc.phase {
				t.Errorf("run_failed.phase = %q, want %q — the setup step is what an operator has to act on", got, tc.phase)
			}
			if tc.wantCode != "" {
				if got, _ := failed.Data["code"].(string); got != string(tc.wantCode) {
					t.Errorf("run_failed.code = %q, want %q — a headless router triages on this field", got, tc.wantCode)
				}
			}
			if tc.wantStatus == store.RunStatusFailedResumable {
				if resumable, _ := failed.Data["resumable"].(bool); !resumable {
					t.Error("run_failed carries no resumable flag on a parked run — a router cannot tell a park from a death")
				}
			}
		})
	}
}

func eventTypes(events []*store.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, string(e.Type))
	}
	return out
}
