package reliability

import (
	"context"
	"regexp"
	"slices"
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
	// Every status that is not a repaired, published output. "unchanged" is
	// the runtime's no-progress stop, which it groups with "exhausted" in its
	// own terminated-episode guard; the empty and unknown rows pin the
	// fail-closed rule, so a status added later cannot read as green here
	// just because nobody came back to update this list.
	for _, status := range []string{"active", "", "some-future-state"} {
		run := &store.Run{
			ExecutionContext:  &store.ExecutionContext{Policy: store.ContextPolicyReport},
			OutputCorrections: map[string]store.OutputCorrectionEpisode{"node": {Status: status}},
		}
		if got := ReportForRun(run); got.RollbackSafe {
			t.Fatalf("%q correction episode must not be rollback-safe: %+v", status, got)
		}
	}
	// The one status that is: correction repaired the output and it shipped.
	if got := ReportForRun(&store.Run{
		ExecutionContext:  &store.ExecutionContext{Policy: store.ContextPolicyReport},
		OutputCorrections: map[string]store.OutputCorrectionEpisode{"node": {Status: "succeeded"}},
	}); !got.RollbackSafe {
		t.Fatalf("a succeeded correction episode must stay rollback-safe: %+v", got)
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
// This is the ratchet for the two defects the plan shipped with: treating
// ITERION_OUTPUT_CORRECTION_BUDGET as a rollout lever even though no production
// path satisfies the correction prerequisites, and an
// ITERION_RELIABILITY_MODE=legacy that the older alias could shadow. Both left
// an operator following the documented procedure still enforcing.
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
	if c.Mode != ModeLegacy || c.ContextPolicy() != store.ContextPolicyLegacy || c.RetryCircuitThreshold != 7 || c.RetryCircuitCooldown != 2*time.Minute {
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
		{name: "unrecognised mode falls back to the alias", mode: "nonsense", alias: "enforce", want: ModeEnforce, policy: store.ContextPolicyEnforce},
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

func TestInvalidModeWarnsOnceAndKeepsValidAlias(t *testing.T) {
	const invalid = "enforced-by-typo"
	key := EnvMode + "\x00" + invalid
	warnedInvalidModes.Delete(key)
	oldWarn := emitInvalidModeWarning
	t.Cleanup(func() {
		emitInvalidModeWarning = oldWarn
		warnedInvalidModes.Delete(key)
	})
	var calls int
	emitInvalidModeWarning = func(name, value string) {
		calls++
		if name != EnvMode || value != invalid {
			t.Fatalf("warning = (%q, %q), want (%q, %q)", name, value, EnvMode, invalid)
		}
	}
	t.Setenv(EnvMode, invalid)
	t.Setenv(EnvContextPolicyAlias, "enforce")
	if got := ModeFromEnv(); got != ModeEnforce {
		t.Fatalf("invalid mode masked valid alias: got %q", got)
	}
	if got := ModeFromEnv(); got != ModeEnforce {
		t.Fatalf("second resolution = %q, want enforce", got)
	}
	if calls != 1 {
		t.Fatalf("invalid mode warnings = %d, want one", calls)
	}
}

// TestSummarizeCountsBlockedAdmissions is the acceptance criterion the
// pilot is judged on: "the pilot report includes the blocked admissions".
// The status counters cannot answer it — a denied run never executes and
// lands in `failed`, where it is indistinguishable from a run that failed
// on its own work — so a promotion decided on the status row alone is
// decided without the refusals the gate produced.
func TestSummarizeCountsBlockedAdmissions(t *testing.T) {
	b := Summarize([]*store.Run{
		{
			Status:           store.RunStatusFailed,
			ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce},
			Admission:        &store.AdmissionDecision{Decision: "denied", Code: "context_mismatch"},
		},
		{
			Status:           store.RunStatusFinished,
			ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport},
			Admission:        &store.AdmissionDecision{Decision: "allowed", Code: "context_match"},
		},
		// Pre-pilot: no context contract, so the gate allowed it by
		// design and the pilot has nothing to say about it.
		{Status: store.RunStatusFinished, Admission: &store.AdmissionDecision{Decision: "allowed", Code: "legacy_context"}},
		// Never gated at all (a run the engine refused earlier, or one
		// written before the gate existed): it must not read as allowed.
		{Status: store.RunStatusQueued},
	})
	if b.AdmissionsDenied != 1 {
		t.Fatalf("admissions_denied = %d, want 1 — a blocked admission is what the pilot report may not hide (baseline = %+v)", b.AdmissionsDenied, b)
	}
	if b.AdmissionsRecorded != 3 {
		t.Fatalf("admissions_recorded = %d, want 3: zero denials over a population the gate never visited is not zero denials over one it did", b.AdmissionsRecorded)
	}
	if b.LegacyContextRuns != 2 {
		t.Fatalf("legacy_context_runs = %d, want 2 (the pre-pilot run and the ungated one)", b.LegacyContextRuns)
	}
}

// TestSummarizeDoesNotReadAnUnknownVerdictAsAllowed pins the closed set on
// the refusing side: the rollout doc's rule one level up is that a missing
// or unrecognised field means pre-pilot, never successful.
func TestSummarizeDoesNotReadAnUnknownVerdictAsAllowed(t *testing.T) {
	b := Summarize([]*store.Run{
		{Status: store.RunStatusFailed, Admission: &store.AdmissionDecision{Decision: "", Code: "partially_written"}},
	})
	if b.AdmissionsRecorded != 1 {
		t.Fatalf("admissions_recorded = %d, want 1: a recorded decision counts even when its verdict is unreadable", b.AdmissionsRecorded)
	}
	if b.AdmissionsDenied != 0 {
		t.Fatalf("admissions_denied = %d, want 0: an unknown verdict is not a denial either — it is named by the gap between recorded and the verdicts that are readable", b.AdmissionsDenied)
	}
}

// TestResolveKeepsTheRollbackLeverAgainstALevel is the load-bearing law of
// the tiered resolution. Step 4 of the rollout doc promises that
// ITERION_RELIABILITY_MODE=legacy stops the pilot; a level that could raise
// the gate back over the operator pulling that lever would make the
// documented rollback a suggestion.
func TestResolveKeepsTheRollbackLeverAgainstALevel(t *testing.T) {
	t.Setenv(EnvMode, string(ModeLegacy))
	t.Setenv(EnvContextPolicyAlias, string(ModeEnforce))
	floor := FloorFromEnv()
	if !floor.Rollback {
		t.Fatalf("floor = %+v, want the explicit legacy recorded as a rollback", floor)
	}
	for _, level := range []Mode{ModeUnset, ModeReport, ModeEnforce} {
		if got := floor.Resolve(level); got != ModeLegacy {
			t.Fatalf("level %q over an explicit rollback = %q, want legacy", level, got)
		}
	}
}

// TestResolveLetsALevelRaiseButNeverLowerTheFloor pins the floor semantics
// G65 asks for: the process variable is the MINIMUM an operator declared,
// not a default a level overwrites. Lowering it from a level would let a
// database row quietly un-enforce a host the operator set to enforce.
func TestResolveLetsALevelRaiseButNeverLowerTheFloor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		level Mode
		want  Mode
	}{
		{name: "a level raises report to enforce", env: "report", level: ModeEnforce, want: ModeEnforce},
		{name: "a level raises the legacy default", env: "", level: ModeReport, want: ModeReport},
		{name: "a level may not lower enforce", env: "enforce", level: ModeReport, want: ModeEnforce},
		{name: "a level may not lower to legacy", env: "report", level: ModeLegacy, want: ModeReport},
		{name: "an unparseable level is not an answer", env: "report", level: Mode("enforced-by-typo"), want: ModeReport},
		{name: "no level leaves the floor alone", env: "enforce", level: ModeUnset, want: ModeEnforce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvMode, tc.env)
			t.Setenv(EnvContextPolicyAlias, "")
			if got := FloorFromEnv().Resolve(tc.level); got != tc.want {
				t.Fatalf("resolve(level=%q, floor=%q) = %q, want %q", tc.level, tc.env, got, tc.want)
			}
		})
	}
}

// TestTheAliasIsNotTheRollbackLever states the one asymmetry on purpose:
// the older variable is the narrower switch this rollout generalises, and
// the doc performs the rollback through the authoritative one. A legacy
// alias is a floor like any other value, so a level may still raise it —
// otherwise every deployment carrying the old variable would be
// un-promotable by a level without an edit the doc never asked for.
func TestTheAliasIsNotTheRollbackLever(t *testing.T) {
	t.Setenv(EnvMode, "")
	t.Setenv(EnvContextPolicyAlias, string(ModeLegacy))
	floor := FloorFromEnv()
	if floor.Rollback {
		t.Fatalf("floor = %+v: only an explicit %s=legacy is the documented lever", floor, EnvMode)
	}
	if got := floor.Resolve(ModeEnforce); got != ModeEnforce {
		t.Fatalf("level enforce over a legacy alias = %q, want enforce", got)
	}
	if got := floor.Resolve(ModeUnset); got != ModeLegacy {
		t.Fatalf("no level over a legacy alias = %q, want legacy (today's resolution, unchanged)", got)
	}
}

// TestModeFromEnvIsTheFloorWithNoLevel keeps the live path and the tiers in
// one piece: the resolution every launch surface shares must BE the tiered
// one with no level, not a second implementation beside it.
func TestModeFromEnvIsTheFloorWithNoLevel(t *testing.T) {
	for _, tc := range []struct{ mode, alias string }{
		{},
		{mode: "report"},
		{mode: "enforce", alias: "legacy"},
		{mode: "legacy", alias: "enforce"},
		{alias: "report"},
		{mode: "nonsense", alias: "enforce"},
	} {
		t.Setenv(EnvMode, tc.mode)
		t.Setenv(EnvContextPolicyAlias, tc.alias)
		if got, want := ModeFromEnv(), FloorFromEnv().Resolve(ModeUnset); got != want {
			t.Fatalf("mode=%q alias=%q: ModeFromEnv = %q, tiered resolution = %q", tc.mode, tc.alias, got, want)
		}
	}
}

// TestLegacyTenantsFromEnvIsCanonical pins the canonical form the report
// prints: trimmed of spaces, empties dropped, deduplicated, sorted — so two
// reports never differ by the operator's own punctuation.
func TestLegacyTenantsFromEnvIsCanonical(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{name: "unset", want: nil},
		{name: "blank means none", raw: "   ", want: nil},
		{name: "one tenant", raw: "team-a", want: []string{"team-a"}},
		{name: "spaces, empties and a duplicate collapse", raw: " b , a ,,a,,", want: []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvLegacyTenants, tc.raw)
			if got := LegacyTenantsFromEnv(); !slices.Equal(got, tc.want) {
				t.Fatalf("LegacyTenantsFromEnv = %q, want %q", got, tc.want)
			}
			if got := FromEnv().LegacyTenants; !slices.Equal(got, tc.want) {
				t.Fatalf("FromEnv().LegacyTenants = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFloorForTenantCapsOnlyListedTenants is the tenant-scoped rollback
// lever: a listed tenant reads as an explicit legacy rollback even under an
// enforcing deployment, every other tenant keeps the process floor, and an
// empty tenant id is NEVER listed — a surface with no tenant in scope
// resolves exactly as the floor alone resolves.
func TestFloorForTenantCapsOnlyListedTenants(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mode, alias     string
		tenants, tenant string
		want            ModeFloor
	}{
		{name: "no env, no list", tenants: "", tenant: "team-a", want: ModeFloor{Mode: ModeLegacy}},
		{name: "enforce floor stands without a list", mode: "enforce", tenants: "", tenant: "team-a", want: ModeFloor{Mode: ModeEnforce}},
		{name: "a listed tenant is capped to legacy", mode: "enforce", tenants: "team-a", tenant: "team-a", want: ModeFloor{Mode: ModeLegacy, Rollback: true}},
		{name: "an unlisted tenant keeps the floor", mode: "enforce", tenants: "team-a", tenant: "team-b", want: ModeFloor{Mode: ModeEnforce}},
		{name: "an empty tenant is never listed", mode: "enforce", tenants: "team-a", tenant: "", want: ModeFloor{Mode: ModeEnforce}},
		{name: "a listed tenant caps an invalid mode too", mode: "nonsense", alias: "report", tenants: "team-a", tenant: "team-a", want: ModeFloor{Mode: ModeLegacy, Rollback: true}},
		{name: "the cap never flows to an unlisted sibling", mode: "enforce", alias: "", tenants: " team-a , team-a ,", tenant: "team-c", want: ModeFloor{Mode: ModeEnforce}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvMode, tc.mode)
			t.Setenv(EnvContextPolicyAlias, tc.alias)
			t.Setenv(EnvLegacyTenants, tc.tenants)
			if got := FloorForTenant(tc.tenant); got != tc.want {
				t.Fatalf("FloorForTenant(%q) with mode=%q alias=%q tenants=%q = %+v, want %+v", tc.tenant, tc.mode, tc.alias, tc.tenants, got, tc.want)
			}
		})
	}
}

// TestContextPolicyForTenantWithoutATenantIsTheEnvResolution keeps the live
// path and the tenant cap in one piece: with no tenant in scope the
// resolution must BE ContextPolicyFromEnv, not a second implementation
// beside it — the same law TestModeFromEnvIsTheFloorWithNoLevel holds one
// level up.
func TestContextPolicyForTenantWithoutATenantIsTheEnvResolution(t *testing.T) {
	for _, tc := range []struct{ mode, alias string }{
		{},
		{mode: "report"},
		{mode: "enforce", alias: "legacy"},
		{mode: "legacy", alias: "enforce"},
		{alias: "report"},
		{mode: "nonsense", alias: "enforce"},
	} {
		t.Setenv(EnvMode, tc.mode)
		t.Setenv(EnvContextPolicyAlias, tc.alias)
		t.Setenv(EnvLegacyTenants, "team-a")
		for _, tenant := range []string{"", "team-zzz"} {
			if got, want := ContextPolicyForTenant(tenant), ContextPolicyFromEnv(); got != want {
				t.Fatalf("mode=%q alias=%q tenant=%q: ContextPolicyForTenant = %q, ContextPolicyFromEnv = %q", tc.mode, tc.alias, tenant, got, want)
			}
		}
	}
}

// TestSummarizeCountsAdmissionsTheGateWouldDeny is the census A15 asks for
// and the dry-run half of the cloud migration note: under legacy and report
// the admission gate records violations it does not refuse, so the number a
// promotion decides on is "how many would enforce deny at admission", not
// only "how many did it". The census counts admission verdicts only —
// resume-time artifact-contract refusals live in per-run events outside
// this sum.
func TestSummarizeCountsAdmissionsTheGateWouldDeny(t *testing.T) {
	b := Summarize([]*store.Run{
		// The two allowances-over-a-violation the census counts.
		{Status: store.RunStatusFinished, Admission: &store.AdmissionDecision{Decision: "allowed", Code: admissionWouldDenyLegacy}},
		{Status: store.RunStatusFinished, Admission: &store.AdmissionDecision{Decision: "allowed", Code: admissionWouldDenyReport}},
		// An actual refusal counts on the denying side, not twice.
		{Status: store.RunStatusFailed, Admission: &store.AdmissionDecision{Decision: "denied", Code: "context_mismatch"}},
		// Clean allowances and the waived hatch are not violations.
		{Status: store.RunStatusFinished, Admission: &store.AdmissionDecision{Decision: "allowed", Code: "context_match"}},
		{Status: store.RunStatusFinished, Admission: &store.AdmissionDecision{Decision: "allowed", Code: "context_store_optional"}},
		// An unreadable verdict is named by the recorded/denied gap, not
		// silently joined to the census.
		{Status: store.RunStatusQueued, Admission: &store.AdmissionDecision{Decision: "", Code: "partially_written"}},
		// Never gated at all.
		{Status: store.RunStatusQueued},
	})
	if b.AdmissionsWouldDeny != 2 {
		t.Fatalf("admissions_would_deny = %d, want 2 — the launches enforce would deny at admission are the population the dry-run census counts (baseline = %+v)", b.AdmissionsWouldDeny, b)
	}
	if b.AdmissionsDenied != 1 {
		t.Fatalf("admissions_denied = %d, want 1: a refusal stays on the denying side", b.AdmissionsDenied)
	}
	if b.AdmissionsRecorded != 6 {
		t.Fatalf("admissions_recorded = %d, want 6", b.AdmissionsRecorded)
	}
}
func TestR2TerminalCorrectionsAreRollbackSafe(t *testing.T) {
	for _, status := range []string{"exhausted", "unchanged"} {
		if report := ReportForRun(&store.Run{OutputCorrections: map[string]store.OutputCorrectionEpisode{"n": {Status: status}}}); !report.RollbackSafe {
			t.Errorf("terminal episode %s blocks rollback", status)
		}
	}
}
