package e2e

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion run --auto-resume N` turns the operator's manual "resume it again"
// ritual into a bounded in-process loop: a run that exits failed_resumable for
// a RETRYABLE cause is re-driven from its checkpoint, up to N times, re-using
// this run's exact launch overrides. What an operator observes is the run
// reaching `finished` on its own, a `run_auto_resumed` event per attempt, and
// upstream nodes NOT being re-executed.
//
// Determinism: the loop's only wait is `usageLimitDelay`, which is clamped by
// the run's retry policy — the same `Retry` field `iterion schedule` sets — so
// a 1ms horizon collapses it without touching product code. USAGE_LIMIT_BLOCKED
// is also the code whose LAYER-1 recipe fails terminally on the first try, so
// the node's call count is an exact attempt counter.
//
// Mutation check: ignore the flag (never call autoResumeLoop) and the recovery
// case never finishes; drop the retryable-code allow-list and the
// WORKSPACE_SAFETY case gets resumed; drop the MaxAttempts bound and the
// exhaustion case loops past 2 attempts.

// autoResumeCounts is the per-node call log the scenario stub keeps.
type autoResumeCounts struct {
	mu     sync.Mutex
	settle int
	flaky  int
}

// runAutoResumeFixture drives auto_resume_mini.bot through the real CLI entry
// point. failFlakyTimes is how many of `flaky`'s calls fail with failCode
// before it starts succeeding (-1 = always fail).
func runAutoResumeFixture(t *testing.T, runID string, attempts, failFlakyTimes int, failCode runtime.ErrorCode) (string, *autoResumeCounts, error) {
	t.Helper()
	// Disable the OAuth-forfait cap probe: enabled, it would reach out to
	// api.anthropic.com whenever the host happens to carry a Claude Code
	// token, which is exactly the non-determinism this layer must not have.
	t.Setenv("ITERION_FORFAIT_CAP_PCT", "0")

	counts := &autoResumeCounts{}
	exec := newScenarioExecutor()
	exec.on("settle", func(_ map[string]any) (map[string]any, error) {
		counts.mu.Lock()
		defer counts.mu.Unlock()
		counts.settle++
		return map[string]any{"value": "settled"}, nil
	})
	exec.on("flaky", func(_ map[string]any) (map[string]any, error) {
		counts.mu.Lock()
		defer counts.mu.Unlock()
		counts.flaky++
		if failFlakyTimes < 0 || counts.flaky <= failFlakyTimes {
			return nil, &runtime.RuntimeError{
				Code:    failCode,
				NodeID:  "flaky",
				Message: "provider window exhausted (fixture)",
			}
		}
		return map[string]any{"value": "recovered"}, nil
	})

	storeDir := t.TempDir()
	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "auto_resume_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Executor:      exec,
		AutoResume:    attempts,
		Retry:         retrypolicy.Policy{MaxWait: "1ms"},
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	return storeDir, counts, err
}

// autoResumeEvents returns the run_auto_resumed events in order.
func autoResumeEvents(t *testing.T, storeDir, runID string) []*store.Event {
	t.Helper()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var out []*store.Event
	for _, e := range events {
		if e.Type == store.EventRunAutoResumed {
			out = append(out, e)
		}
	}
	return out
}

func TestRunAutoResumeRecoversRetryableFailure(t *testing.T) {
	storeDir, counts, err := runAutoResumeFixture(t, "auto-resume-ok", 2, 1, runtime.ErrCodeUsageLimitBlocked)
	if err != nil {
		t.Fatalf("run: %v — the auto-resume loop did not recover the run", err)
	}
	if got := loadRun(t, storeDir, "auto-resume-ok").Status; got != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", got)
	}
	if counts.flaky != 2 {
		t.Errorf("flaky executed %d time(s), want 2 (one failure + one auto-resumed retry)", counts.flaky)
	}
	if counts.settle != 1 {
		t.Errorf("settle executed %d time(s), want 1 — the resume restarted the workflow instead of continuing from the checkpoint", counts.settle)
	}

	// The recovered output is what downstream consumers read: a resume that
	// kept the failed node's stale (absent) artifact would be invisible here.
	s, serr := store.New(storeDir)
	if serr != nil {
		t.Fatalf("open store: %v", serr)
	}
	art, aerr := s.LoadLatestArtifact(context.Background(), "auto-resume-ok", "flaky")
	if aerr != nil {
		t.Fatalf("load flaky artifact: %v", aerr)
	}
	if art.Data["value"] != "recovered" {
		t.Errorf("flaky artifact = %v, want the retry's output", art.Data)
	}

	evts := autoResumeEvents(t, storeDir, "auto-resume-ok")
	if len(evts) != 1 {
		t.Fatalf("got %d run_auto_resumed events, want 1 — the automation left no trace on the timeline", len(evts))
	}
	if got := evts[0].Data["code"]; got != string(runtime.ErrCodeUsageLimitBlocked) {
		t.Errorf("event code = %v, want %s", got, runtime.ErrCodeUsageLimitBlocked)
	}
	if got := evts[0].Data["attempt"]; toInt(got) != 1 {
		t.Errorf("event attempt = %v, want 1", got)
	}
	if got := evts[0].Data["max"]; toInt(got) != 2 {
		t.Errorf("event max = %v, want the --auto-resume budget 2", got)
	}
}

func TestRunAutoResumeStaysOffByDefault(t *testing.T) {
	storeDir, counts, err := runAutoResumeFixture(t, "auto-resume-off", 0, 1, runtime.ErrCodeUsageLimitBlocked)
	if err == nil {
		t.Fatal("run finished without --auto-resume: the loop ran unasked")
	}
	if got := loadRun(t, storeDir, "auto-resume-off").Status; got != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable", got)
	}
	if counts.flaky != 1 {
		t.Errorf("flaky executed %d time(s), want 1 — the run was resumed without the flag", counts.flaky)
	}
	if n := len(autoResumeEvents(t, storeDir, "auto-resume-off")); n != 0 {
		t.Errorf("got %d run_auto_resumed events, want 0", n)
	}
}

func TestRunAutoResumeRefusesNonRetryableCause(t *testing.T) {
	// WORKSPACE_SAFETY is a deterministic wall: resuming would re-hit it, so
	// the loop must leave the run for manual review.
	storeDir, counts, err := runAutoResumeFixture(t, "auto-resume-terminal", 3, -1, runtime.ErrCodeWorkspaceSafety)
	if err == nil {
		t.Fatal("run finished, want the non-retryable failure to stand")
	}
	if got := loadRun(t, storeDir, "auto-resume-terminal").Status; got != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable", got)
	}
	if counts.flaky != 1 {
		t.Errorf("flaky executed %d time(s), want 1 — a non-retryable cause was auto-resumed anyway", counts.flaky)
	}
	if n := len(autoResumeEvents(t, storeDir, "auto-resume-terminal")); n != 0 {
		t.Errorf("got %d run_auto_resumed events, want 0", n)
	}
}

func TestRunAutoResumeStopsAtTheAttemptBudget(t *testing.T) {
	storeDir, counts, err := runAutoResumeFixture(t, "auto-resume-exhausted", 2, -1, runtime.ErrCodeUsageLimitBlocked)
	if err == nil {
		t.Fatal("run finished, want the permanently-failing node to exhaust the budget")
	}
	if got := loadRun(t, storeDir, "auto-resume-exhausted").Status; got != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable (the operator resumes manually once the cause is fixed)", got)
	}
	if counts.flaky != 3 {
		t.Errorf("flaky executed %d time(s), want 3 (initial + 2 auto-resumes) — the budget is not bounding the loop", counts.flaky)
	}
	evts := autoResumeEvents(t, storeDir, "auto-resume-exhausted")
	if len(evts) != 2 {
		t.Fatalf("got %d run_auto_resumed events, want exactly 2", len(evts))
	}
	for i, e := range evts {
		if got := toInt(e.Data["attempt"]); got != i+1 {
			t.Errorf("event %d attempt = %d, want %d", i, got, i+1)
		}
	}
}
