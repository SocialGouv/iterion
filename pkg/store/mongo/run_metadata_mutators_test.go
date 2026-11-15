package mongo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRunMetadataPublicMutatorsRefuseFuture(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
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
		status store.RunStatus
		probe  bool
		apply  func(context.Context, string) error
	}{
		{"correction-direct", "", false, func(c context.Context, id string) error {
			return s.SetRunOutputCorrection(c, id, "node", store.OutputCorrectionEpisode{Generation: "g", Revision: 1, Status: "active", Budget: 1})
		}},
		{"patch-budget", "", false, func(c context.Context, id string) error {
			return s.PatchRunBudgetOverrides(c, id, &store.RunBudgetOverrides{MaxTokens: 12})
		}},
		{"set-budget", "", false, func(c context.Context, id string) error {
			return s.SetRunBudgetOverrides(c, id, &store.RunBudgetOverrides{MaxTokens: 12})
		}},
		{"budget-snapshot", "", false, func(c context.Context, id string) error {
			return s.SetRunBudgetSnapshot(c, id, &store.RunBudget{MaxTokens: 12})
		}},
		{"steering", "", false, func(c context.Context, id string) error {
			return s.PatchRunSteering(c, id, map[string]int{"loop": 3}, &store.RunBudgetRaises{MaxTokens: 13})
		}},
		{"permissions", "", false, func(c context.Context, id string) error {
			return s.PatchRunPermissionGrants(c, id, map[string][]string{"n": {"read"}})
		}},
		{"node-served", "", false, func(c context.Context, id string) error {
			return s.RecordNodeServed(c, id, "n", store.NodeServed{Backend: "claw", Model: "test"})
		}},
		{"credential-stamp", "", false, func(c context.Context, id string) error {
			return s.SetRunCredStamp(c, id, store.RunCredStamp{Fingerprints: []string{"test-fingerprint"}})
		}},
		{"source", "", false, func(c context.Context, id string) error {
			return s.SetRunRecordedSource(c, id, "bot source", nil, "hash")
		}},
		{"idle", "", false, func(c context.Context, id string) error { return s.SetRunLLMIdle(c, id, &at) }},
		{"runner-version", "", false, func(c context.Context, id string) error { return s.SetRunnerVersion(c, id, "test-version") }},
		{"watcher", "", false, func(c context.Context, id string) error {
			return s.SetWatcherCursor(c, id, "w", store.WatcherCursor{ProgressSequence: 1})
		}},
		{"await-answers", "", false, func(c context.Context, id string) error {
			return s.SetAwaitAnswersWait(c, id, "token", &store.AwaitAnswersWait{NodeID: "n", Until: at.Add(time.Hour)})
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
			return s.UpdateRunStatus(c, id, store.RunStatusFailedResumable, "failed")
		}},
		{"status-if", store.RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.UpdateRunStatusIf(c, id, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}))
		}},
		{"outcome", "", false, func(c context.Context, id string) error {
			return changed(s.UpdateRunOutcome(c, id, store.RunStatusFailedResumable, "failed", store.RunOutcomeMeta{}, nil))
		}},
		{"claim-merge", "", false, func(c context.Context, id string) error {
			ok, _, _, e := s.ClaimMerge(c, id, at.Add(-time.Hour))
			return changed(ok, e)
		}},
		{"merge-if", "", false, func(c context.Context, id string) error {
			return changed(s.UpdateRunMergeIf(c, id, store.RunMergeUpdate{Status: store.MergeStatusFailed}, []store.MergeStatus{store.MergeStatusPending}))
		}},
		{"flip", store.RunStatusFailedResumable, false, func(c context.Context, id string) error {
			_, ok, e := s.FlipToQueued(c, id, store.RunStatusFailedResumable, at)
			return changed(ok, e)
		}},
		{"revert-flip", store.RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.RevertQueuedFlip(c, id, store.QueuedFlip{At: at, Prior: store.QueuedFlipPrior{Status: store.RunStatusFailedResumable}}, "failed"))
		}},
		{"fail-queued", store.RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.FailQueuedRunIfAttempt(c, id, "failed", at, store.RunOutcomeMeta{}))
		}},
		{"release-queued", store.RunStatusQueued, false, func(c context.Context, id string) error {
			return changed(s.ReleaseQueuedRunIfAttempt(c, id, store.RunStatusFailedResumable, "failed", at, store.RunOutcomeMeta{}))
		}},
		{"schedule-retry", store.RunStatusFailedResumable, false, func(c context.Context, id string) error {
			ok, _, e := s.ScheduleRunRetry(c, id, at.Add(time.Hour), at, "reason", "code", 3)
			return changed(ok, e)
		}},
		{"delay-retry", store.RunStatusFailedResumable, false, func(c context.Context, id string) error {
			return changed(s.DelayRunRetry(c, id, at, at.Add(time.Hour), "reason", "code"))
		}},
		{"pull-retry", store.RunStatusFailedResumable, false, func(c context.Context, id string) error {
			return changed(s.PullRunRetry(c, id, at, at.Add(-time.Hour)))
		}},
		{"rearm-retry", store.RunStatusFailedResumable, true, func(c context.Context, id string) error {
			return changed(s.RearmProbedRunRetry(c, id, at.Add(time.Hour), at))
		}},
		{"claim-retry", store.RunStatusFailedResumable, false, func(c context.Context, id string) error { return changed(s.ClaimRunRetry(c, id, at)) }},
		{"clear-retry", store.RunStatusFailedResumable, false, func(c context.Context, id string) error { return s.ClearRunRetry(c, id) }},
		{"abandon-retry", store.RunStatusFailedResumable, false, func(c context.Context, id string) error { return changed(s.AbandonRunRetry(c, id, "final", at)) }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			for _, future := range []bool{false, true} {
				id := fmt.Sprintf("%s-%t", tt.name, future)
				status := tt.status
				if status == "" {
					status = store.RunStatusRunning
				}
				if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, Format: 2, Status: store.RunStatusRunning}); err != nil {
					t.Fatal(err)
				}
				set := bson.M{"status": status, "queued_at": at, "merge_status": store.MergeStatusPending, "watched_issue_ids": bson.A{"first"}, "subbot_children": bson.M{"n": "child"}, "retry_state.retry_after": at}
				if tt.probe {
					set["retry_state.probe_from"] = at
				}
				if future {
					set["format_version"] = 99
				}
				if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": set}); err != nil {
					t.Fatal(err)
				}
				before, err := s.runs.FindOne(ctx, bson.M{"_id": id}).Raw()
				if err != nil {
					t.Fatal(err)
				}
				err = tt.apply(ctx, id)
				after, readErr := s.runs.FindOne(ctx, bson.M{"_id": id}).Raw()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if future {
					if !errors.Is(err, store.ErrUnsupportedValueFormat) || !bytes.Equal(before, after) {
						t.Fatalf("future call err=%v changed=%t", err, !bytes.Equal(before, after))
					}
				} else if err != nil || bytes.Equal(before, after) {
					t.Fatalf("known2 control err=%v changed=%t", err, !bytes.Equal(before, after))
				}
			}
		})
	}
}
