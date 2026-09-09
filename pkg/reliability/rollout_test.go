package reliability

import (
	"context"
	"regexp"
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

// TestReportForRunCountsPublishingNodesNotArtifactVersions pins what the
// artifact index can actually back. It builds the index through the REAL
// store rather than by hand, because a hand-written map is exactly what let
// the field be named published_artifact_count while holding a publisher
// count: store.Run.ArtifactIndex keeps the LATEST version per node, so a node
// that published three versions still contributes one entry.
func TestReportForRunCountsPublishingNodesNotArtifactVersions(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(ctx, "run-artifacts", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Two publishing nodes, four artifact versions between them.
	for _, a := range []store.Artifact{
		{RunID: "run-artifacts", NodeID: "plan", Version: 0, Data: map[string]any{"n": 0}},
		{RunID: "run-artifacts", NodeID: "plan", Version: 1, Data: map[string]any{"n": 1}},
		{RunID: "run-artifacts", NodeID: "plan", Version: 2, Data: map[string]any{"n": 2}},
		{RunID: "run-artifacts", NodeID: "verdict", Version: 0, Data: map[string]any{"n": 0}},
	} {
		if err := st.WriteArtifact(ctx, &a); err != nil {
			t.Fatalf("WriteArtifact %s/%d: %v", a.NodeID, a.Version, err)
		}
	}
	run, err := st.LoadRun(ctx, "run-artifacts")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got := ReportForRun(run).PublishingNodeCount; got != 2 {
		t.Fatalf("PublishingNodeCount = %d, want 2 (distinct publishers, not the 4 versions written)", got)
	}
}

func TestReportForRunComputesRollbackSafety(t *testing.T) {
	if got := ReportForRun(nil); got.RollbackSafe {
		t.Fatalf("nil run must not be rollback-safe: %+v", got)
	}
	for _, status := range []string{"active", "exhausted", "unchanged"} {
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
		// BOTH paused statuses, so the bucket cannot stay green while
		// counting only the one an author happened to think of — the
		// drift the store's IsPaused contract exists to prevent.
		{Status: store.RunStatusPausedWaitingHuman},
	})
	if b.Total != 5 || b.Finished != 1 || b.Failed != 1 || b.FailedResumable != 1 || b.Paused != 2 || b.RetryArmed != 1 || b.RunsWithWatcherCursors != 1 {
		t.Fatalf("baseline = %+v", b)
	}
	plan := (Config{}).Rollback()
	if !plan.DisableEnforcement || !plan.PreserveEvidence || len(plan.Actions) == 0 {
		t.Fatalf("rollback = %+v", plan)
	}
}

// envAssignment matches the `set NAME=VALUE` shape RollbackPlan.Actions uses.
var envAssignment = regexp.MustCompile(`\bset ([A-Z][A-Z0-9_]*)=(\S+)`)

// TestRollbackPlanActionsActuallyFire executes the published plan verbatim
// against a hostile baseline and checks the observable outcome, because a
// plan is only an emergency lever if pulling it changes something.
//
// This is the ratchet for the two defects the plan shipped with: an action
// naming ITERION_OUTPUT_CORRECTION_BUDGET, which no production path reads, and
// an ITERION_RELIABILITY_MODE=legacy that the older alias could shadow. Both
// left an operator following the documented procedure still enforcing.
func TestRollbackPlanActionsActuallyFire(t *testing.T) {
	// A deployment mid-pilot, enforcing through the new switch AND the older
	// alias — the host on which the rollback has the most to undo.
	t.Setenv(EnvMode, string(ModeEnforce))
	t.Setenv(EnvContextPolicyAlias, string(ModeEnforce))
	if got := ContextPolicyFromEnv(); got != store.ContextPolicyEnforce {
		t.Fatalf("baseline policy = %s, want enforce; the rollback has nothing to undo", got)
	}

	// Only variables this package resolves may be named: an action that sets
	// anything else reads as a lever and does nothing.
	readsEnv := map[string]bool{EnvMode: true, EnvContextPolicyAlias: true}
	applied := 0
	for _, action := range (Config{}).Rollback().Actions {
		for _, m := range envAssignment.FindAllStringSubmatch(action, -1) {
			if !readsEnv[m[1]] {
				t.Errorf("action %q sets %s, which no rollout resolution reads", action, m[1])
				continue
			}
			t.Setenv(m[1], m[2])
			applied++
		}
	}
	if applied == 0 {
		t.Fatal("no rollback action assigns a variable the rollout reads; the plan cannot be executed")
	}
	if got := ContextPolicyFromEnv(); got != store.ContextPolicyLegacy {
		t.Fatalf("policy after executing the rollback plan = %s, want legacy", got)
	}
}

func TestFromEnvDefaultsToSafeLegacy(t *testing.T) {
	t.Setenv(EnvMode, "")
	t.Setenv(EnvContextPolicyAlias, "")
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
	t.Setenv("ITERION_EXECUTION_CONTEXT_POLICY", "report")
	if got := FromEnv().ContextPolicy(); got != store.ContextPolicyEnforce {
		t.Fatalf("reliability mode must win over older policy = %s", got)
	}
	t.Setenv(EnvMode, "")
	if got := FromEnv().ContextPolicy(); got != store.ContextPolicyReport {
		t.Fatalf("older policy fallback = %s", got)
	}
	t.Setenv(EnvOutputCorrectionBudget, " 0 ")
	if got := FromEnv().OutputCorrectionBudget; got != 0 {
		t.Fatalf("spaced correction budget = %d, want 0", got)
	}
}

// TestModeFromEnvPrecedence covers every combination of the two variables an
// operator can be carrying. The load-bearing row is "legacy over an enforcing
// alias": the rollback documented in docs/workflow-reliability-1006.md is only
// an emergency lever if the older variable cannot shadow it.
func TestModeFromEnvPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		alias  string
		want   Mode
		policy store.ContextPolicy
	}{
		{name: "neither set", want: ModeLegacy, policy: store.ContextPolicyLegacy},
		{name: "mode alone", mode: "enforce", want: ModeEnforce, policy: store.ContextPolicyEnforce},
		{name: "alias alone", alias: "report", want: ModeReport, policy: store.ContextPolicyReport},
		{name: "legacy over an enforcing alias", mode: "legacy", alias: "enforce", want: ModeLegacy, policy: store.ContextPolicyLegacy},
		{name: "report over an enforcing alias", mode: "report", alias: "enforce", want: ModeReport, policy: store.ContextPolicyReport},
		{name: "enforce over a legacy alias", mode: "enforce", alias: "legacy", want: ModeEnforce, policy: store.ContextPolicyEnforce},
		{name: "agreeing values", mode: "report", alias: "report", want: ModeReport, policy: store.ContextPolicyReport},
		{name: "whitespace and case are tolerated", mode: "  Enforce\t", want: ModeEnforce, policy: store.ContextPolicyEnforce},
		{name: "blank mode falls through to the alias", mode: "   ", alias: "enforce", want: ModeEnforce, policy: store.ContextPolicyEnforce},
		{name: "unrecognised mode means legacy, not the alias", mode: "nonsense", alias: "enforce", want: ModeLegacy, policy: store.ContextPolicyLegacy},
		{name: "unrecognised alias means legacy", alias: "nonsense", want: ModeLegacy, policy: store.ContextPolicyLegacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvMode, tc.mode)
			t.Setenv(EnvContextPolicyAlias, tc.alias)
			if got := ModeFromEnv(); got != tc.want {
				t.Fatalf("mode = %q, want %q", got, tc.want)
			}
			if got := ContextPolicyFromEnv(); got != tc.policy {
				t.Fatalf("policy = %q, want %q", got, tc.policy)
			}
			if got := FromEnv().Mode; got != tc.want {
				t.Fatalf("FromEnv mode = %q, want %q", got, tc.want)
			}
		})
	}
}
