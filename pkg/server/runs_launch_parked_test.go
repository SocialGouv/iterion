package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// submissionPublisher answers every launch with one fixed submission — the
// cloud publisher's word on where the run landed.
type submissionPublisher struct{ sub runview.LaunchSubmission }

func (p submissionPublisher) SubmitLaunch(context.Context, string, runview.LaunchSpec, *ir.Workflow, *runview.CompiledSource) (runview.LaunchSubmission, error) {
	return p.sub, nil
}

func (submissionPublisher) CancelRun(context.Context, string) error { return nil }

func (submissionPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}

func (submissionPublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	return nil
}

// The launch response reports the status the publisher persisted the run
// in. A launch PARKED on a credential's hard concurrency cap is written
// failed_resumable under CRED_SLOT_WAIT with nothing on the queue: answering
// "running" for it — the zero queue position's reading — tells a CLI, a
// webhook or the studio the run runs while it waits for a slot. A queued
// run reports queued whether or not the position lookup landed. Red when
// the handler reads the status off the queue position alone.
func TestLaunch_TheResponseReportsTheStatusThePublisherPersisted(t *testing.T) {
	cases := []struct {
		name       string
		sub        runview.LaunchSubmission
		wantStatus store.RunStatus
		wantPos    int
	}{
		{"parked on a credential slot", runview.LaunchSubmission{Status: store.RunStatusFailedResumable, FailureCode: store.FailureCredSlotWait}, store.RunStatusFailedResumable, 0},
		{"queued with its position", runview.LaunchSubmission{Status: store.RunStatusQueued, QueuePosition: 2}, store.RunStatusQueued, 2},
		{"queued, position lookup failed", runview.LaunchSubmission{Status: store.RunStatusQueued}, store.RunStatusQueued, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgTestServer(t)
			ctx := seedGate(t, s, gateSpec{id: "t1"})
			rs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.Store = rs
			s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(submissionPublisher{sub: c.sub}))

			rec := postLaunch(t, s, ctx, `{"file_path":"x.bot","source":"workflow probe:\n  entry: done\n"}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
			}
			var out launchRunResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if out.Status != string(c.wantStatus) || out.QueuePosition != c.wantPos {
				t.Fatalf("launch answered status=%q position=%d, want %q/%d — the response must report the status the publisher persisted the run in", out.Status, out.QueuePosition, c.wantStatus, c.wantPos)
			}
		})
	}
}
