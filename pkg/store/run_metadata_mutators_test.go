package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"os"
	"strings"
)

func TestRunMetadataPublicFilesystemMutatorsRefuseFuture(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	at := time.Now().UTC().Truncate(time.Millisecond)
	changed := func(ok bool, err error) error {
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("positive control did not match")
		}
		return nil
	}
	cases := []struct {
		name   string
		status RunStatus
		probe  bool
		apply  func(context.Context, string) error
	}{
		{"correction-direct", "", false, func(c context.Context, id string) error {
			return s.SetRunOutputCorrection(c, id, "node", OutputCorrectionEpisode{Generation: "g", Revision: 1, Status: "active", Budget: 1})
		}},
		{"patch-budget", "", false, func(c context.Context, id string) error {
			return s.PatchRunBudgetOverrides(c, id, &RunBudgetOverrides{MaxTokens: 12})
		}},
		{"set-budget", "", false, func(c context.Context, id string) error {
			return s.SetRunBudgetOverrides(c, id, &RunBudgetOverrides{MaxTokens: 12})
		}},
		{"budget-snapshot", "", false, func(c context.Context, id string) error {
			return s.SetRunBudgetSnapshot(c, id, &RunBudget{MaxTokens: 12})
		}},
		{"steering", "", false, func(c context.Context, id string) error {
			return s.PatchRunSteering(c, id, map[string]int{"loop": 3}, &RunBudgetRaises{MaxTokens: 13})
		}},
		{"permissions", "", false, func(c context.Context, id string) error {
			return s.PatchRunPermissionGrants(c, id, map[string][]string{"n": {"read"}})
		}},
		{"node-served", "", false, func(c context.Context, id string) error {
			return s.RecordNodeServed(c, id, "n", NodeServed{Backend: "claw", Model: "test"})
		}},
		{"credential-stamp", "", false, func(c context.Context, id string) error {
			return s.SetRunCredStamp(c, id, RunCredStamp{Fingerprints: []string{"test-fingerprint"}})
		}},
		{"source", "", false, func(c context.Context, id string) error {
			return s.SetRunRecordedSource(c, id, "bot source", nil, "hash")
		}},
		{"idle", "", false, func(c context.Context, id string) error { return s.SetRunLLMIdle(c, id, &at) }},
		{"runner-version", "", false, func(c context.Context, id string) error { return s.SetRunnerVersion(c, id, "test-version") }},
		{"watcher", "", false, func(c context.Context, id string) error {
			return s.SetWatcherCursor(c, id, "w", WatcherCursor{ProgressSequence: 1})
		}},
		{"await-answers", "", false, func(c context.Context, id string) error {
			return s.SetAwaitAnswersWait(c, id, "token", &AwaitAnswersWait{NodeID: "n", Until: at.Add(time.Hour)})
		}},
		{"add-watched", "", false, func(c context.Context, id string) error {
			_, e := s.AddWatchedIssues(c, id, []string{"second"})
			return e
		}},
		{"remove-watched", "", false, func(c context.Context, id string) error {
			_, e := s.RemoveWatchedIssues(c, id, []string{"first"})
			return e
		}},
		{"subbot-child", "", false, func(c context.Context, id string) error { return s.SetSubbotChild(c, id, "n", "new-child") }},
		{"clear-subbot-child", "", false, func(c context.Context, id string) error { return s.ClearSubbotChild(c, id, "n") }},
		{"status", "", false, func(c context.Context, id string) error {
			return s.UpdateRunStatus(c, id, RunStatusFailedResumable, "failed")
		}},
		{"status-if", RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.UpdateRunStatusIf(c, id, RunStatusRunning, "", []RunStatus{RunStatusQueued}))
		}},
		{"outcome", "", false, func(c context.Context, id string) error {
			return changed(s.UpdateRunOutcome(c, id, RunStatusFailedResumable, "failed", RunOutcomeMeta{}, nil))
		}},
		{"claim-merge", "", false, func(c context.Context, id string) error {
			ok, _, _, e := s.ClaimMerge(c, id, at.Add(-time.Hour))
			return changed(ok, e)
		}},
		{"merge-if", "", false, func(c context.Context, id string) error {
			return changed(s.UpdateRunMergeIf(c, id, RunMergeUpdate{Status: MergeStatusFailed}, []MergeStatus{MergeStatusPending}))
		}},
		{"flip", RunStatusFailedResumable, false, func(c context.Context, id string) error {
			_, ok, e := s.FlipToQueued(c, id, RunStatusFailedResumable, at)
			return changed(ok, e)
		}},
		{"revert-flip", RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.RevertQueuedFlip(c, id, QueuedFlip{At: at, Prior: QueuedFlipPrior{Status: RunStatusFailedResumable}}, "failed"))
		}},
		{"fail-queued", RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.FailQueuedRunIfAttempt(c, id, "failed", at, RunOutcomeMeta{}))
		}},
		{"release-queued", RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.ReleaseQueuedRunIfAttempt(c, id, RunStatusFailedResumable, "failed", at, RunOutcomeMeta{}))
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			for _, future := range []bool{false, true} {
				id := fmt.Sprintf("%s-%t", tt.name, future)
				status := tt.status
				if status == "" {
					status = RunStatusRunning
				}
				if _, err := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: id, Format: 2, Status: RunStatusRunning}); err != nil {
					t.Fatal(err)
				}
				run, err := s.LoadRun(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				run.Status = status
				run.QueuedAt = &at
				run.MergeStatus = MergeStatusPending
				run.WatchedIssueIDs = []string{"first"}
				run.SubbotChildren = map[string]string{"n": "child"}
				bound, err := WithRunValueFormat(ctx, RunValueFormat{RunID: id, Format: 2})
				if err != nil {
					t.Fatal(err)
				}
				if err = s.SaveRun(bound, run); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(s.runJSONPath(id))
				if err != nil {
					t.Fatal(err)
				}
				if future {
					before = []byte(strings.Replace(string(before), `"format_version": 2`, `"format_version": 99`, 1))
					if err = os.WriteFile(s.runJSONPath(id), before, 0600); err != nil {
						t.Fatal(err)
					}
				}
				err = tt.apply(ctx, id)
				after, readErr := os.ReadFile(s.runJSONPath(id))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if future {
					if !errors.Is(err, ErrUnsupportedValueFormat) || !bytes.Equal(before, after) {
						t.Fatalf("future call err=%v changed=%t", err, !bytes.Equal(before, after))
					}
				} else if err != nil || bytes.Equal(before, after) {
					t.Fatalf("known2 control err=%v changed=%t", err, !bytes.Equal(before, after))
				}
			}
		})
	}
}
