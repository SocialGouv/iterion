package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

func TestPreparedChildCancellationDoesNotInheritParentGrant(t *testing.T) {
	for _, mode := range []string{"direct", "entry", "resume-early-refusal"} {
		t.Run(mode, func(t *testing.T) {
			s, p := preparedFixture(t)
			parent, e := s.CreateRun(t.Context(), "parent", "parent", nil)
			if e != nil {
				t.Fatal(e)
			}
			authority, ok, e := s.AcquireRunExecutionIf(t.Context(), "parent", store.RunExecutionExpectation{Version: parent.CASVersion, Epoch: parent.ExecutionEpoch, Status: parent.Status}, store.ExecutionStart)
			if e != nil || !ok {
				t.Fatalf("parent acquisition %v %v", ok, e)
			}
			grant := &correctionExecution{runs: s, authority: authority}
			ctx := p.Context
			if mode == "entry" {
				ctx = context.WithValue(ctx, correctionEntryKey{}, &correctionEntry{grant: grant})
			} else {
				ctx = context.WithValue(ctx, correctionExecutionKey{}, grant)
			}
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			calls := 0
			engine := admissionSnapshotEngine(t, s, nil, &calls)
			if mode == "resume-early-refusal" {
				if e = s.UpdateRunStatus(t.Context(), "run", store.RunStatusPausedOperator, ""); e != nil {
					t.Fatal(e)
				}
				e = engine.Resume(ctx, "run", nil)
			} else {
				e = engine.RunPrepared(ctx, "run", nil, p)
			}
			child, re := s.FilesystemRunStore.LoadRun(t.Context(), "run")
			if re != nil {
				t.Fatal(re)
			}
			parent, re = s.FilesystemRunStore.LoadRun(t.Context(), "parent")
			if re != nil {
				t.Fatal(re)
			}
			if !authority.Matches(parent) || parent.Status != store.RunStatusRunning || calls != 0 {
				t.Fatalf("parent authority/state changed or executor ran: %+v calls=%d", parent, calls)
			}
			if mode == "resume-early-refusal" {
				if !errors.Is(e, context.Canceled) || child.Status != store.RunStatusPausedOperator || s.writes != 0 {
					t.Fatalf("resume cancellation mutated state: %s %v writes=%d", child.Status, e, s.writes)
				}
			} else if !errors.Is(e, ErrRunCancelled) || child.Status != store.RunStatusCancelled || s.writes != 1 {
				t.Fatalf("parent grant stranded child: status=%s err=%v writes=%d", child.Status, e, s.writes)
			}
		})
	}
}
