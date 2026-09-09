package reliability

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestReportForRunDistinguishesLegacyAndContractRuns(t *testing.T) {
	legacy := ReportForRun(&store.Run{ID: "legacy", ArtifactIndex: map[string]int{"a": 1}})
	if !legacy.LegacyContext || legacy.ContextPolicy != string(store.ContextPolicyLegacy) || !legacy.RollbackSafe {
		t.Fatalf("legacy report = %+v", legacy)
	}
	contract := ReportForRun(&store.Run{
		ID:                "contract",
		ExecutionContext:  &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport},
		Admission:         &store.AdmissionDecision{Decision: "allow"},
		ArtifactIndex:     map[string]int{"a": 1},
		OutputCorrections: map[string]store.OutputCorrectionEpisode{"a": {Status: "succeeded"}},
		WatcherCursors:    map[string]store.WatcherCursor{"watch": {WatcherID: "watch"}},
	})
	if contract.LegacyContext || contract.ContextVersion != 1 || contract.ContextPolicy != string(store.ContextPolicyReport) || contract.CorrectionEpisodeCount != 1 || contract.WatcherCursorCount != 1 {
		t.Fatalf("contract report = %+v", contract)
	}
}

func TestReportForRunComputesRollbackSafety(t *testing.T) {
	if got := ReportForRun(nil); got.RollbackSafe {
		t.Fatalf("nil run must not be rollback-safe: %+v", got)
	}
	for _, status := range []string{"active", "exhausted"} {
		run := &store.Run{
			ExecutionContext:  &store.ExecutionContext{Policy: store.ContextPolicyReport},
			OutputCorrections: map[string]store.OutputCorrectionEpisode{"node": {Status: status}},
		}
		if got := ReportForRun(run); got.RollbackSafe {
			t.Fatalf("%s correction episode must not be rollback-safe: %+v", status, got)
		}
	}
	if got := ReportForRun(&store.Run{
		ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyEnforce},
	}); got.RollbackSafe {
		t.Fatalf("enforced run must not be rollback-safe: %+v", got)
	}
}

func TestSummarizeAndRollback(t *testing.T) {
	at := time.Now().Add(time.Hour)
	b := Summarize([]*store.Run{
		{Status: store.RunStatusFinished},
		{Status: store.RunStatusFailed},
		{Status: store.RunStatusFailedResumable, RetryState: &store.RunRetryState{RetryAfter: &at}},
		{Status: store.RunStatusPausedOperator, WatcherCursors: map[string]store.WatcherCursor{"w": {}}},
	})
	if b.Total != 4 || b.Finished != 1 || b.Failed != 1 || b.FailedResumable != 1 || b.Paused != 1 || b.RetryArmed != 1 || b.RunsWithWatcherCursors != 1 {
		t.Fatalf("baseline = %+v", b)
	}
	plan := (Config{}).Rollback()
	if !plan.DisableEnforcement || !plan.PreserveEvidence || len(plan.Actions) == 0 {
		t.Fatalf("rollback = %+v", plan)
	}
}

func TestFromEnvDefaultsToSafeLegacy(t *testing.T) {
	t.Setenv(EnvMode, "")
	t.Setenv("ITERION_RETRY_CIRCUIT_THRESHOLD", "7")
	t.Setenv("ITERION_RETRY_CIRCUIT_COOLDOWN", "2m")
	c := FromEnv()
	if c.Mode != ModeLegacy || c.ContextPolicy() != store.ContextPolicyLegacy || !c.WatcherCursorsEnabled || c.RetryCircuitThreshold != 7 || c.RetryCircuitCooldown != 2*time.Minute {
		t.Fatalf("config = %+v", c)
	}
	t.Setenv(EnvMode, "enforce")
	if got := FromEnv().ContextPolicy(); got != store.ContextPolicyEnforce {
		t.Fatalf("enforce policy = %s", got)
	}
}
