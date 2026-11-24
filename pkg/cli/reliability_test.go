package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/reliability"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedReliabilityStore builds a store with one finished and one
// human-paused run, through the REAL store so the report is exercised
// against run documents the engine would actually have written.
func seedReliabilityStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	for id, status := range map[string]store.RunStatus{
		"run-done":   store.RunStatusFinished,
		"run-paused": store.RunStatusPausedWaitingHuman,
	} {
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if err := s.UpdateRunStatus(ctx, id, status, ""); err != nil {
			t.Fatalf("UpdateRunStatus %s: %v", id, err)
		}
	}
	return dir
}

func reliabilityJSON(t *testing.T, opts ReliabilityOptions) ReliabilityReport {
	t.Helper()
	var buf bytes.Buffer
	if err := RunReliabilityReport(context.Background(), opts, &Printer{W: &buf, Format: OutputJSON}); err != nil {
		t.Fatalf("RunReliabilityReport: %v", err)
	}
	var got ReliabilityReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return got
}

// TestReliabilityReportBaselineCountsTheStore is the operator-facing half
// of docs/workflow-reliability-1006.md step 1: the baseline has to be
// obtainable without a Go compiler, and it has to count what is on disk.
func TestReliabilityReportBaselineCountsTheStore(t *testing.T) {
	t.Setenv(reliability.EnvMode, "")
	t.Setenv(reliability.EnvContextPolicyAlias, "")
	got := reliabilityJSON(t, ReliabilityOptions{StoreDir: seedReliabilityStore(t)})
	if got.Baseline == nil {
		t.Fatal("no baseline in the report")
	}
	if got.Baseline.Total != 2 || got.Baseline.Finished != 1 || got.Baseline.Paused != 1 {
		t.Fatalf("baseline = %+v", *got.Baseline)
	}
	if got.Run != nil {
		t.Fatalf("fleet baseline must not carry a single-run report: %+v", got.Run)
	}
}

// TestReliabilityReportNamesThePolicyLaunchesApply is the ratchet for the
// defect class this command reports on: a surface that re-derives the
// rollout mode can name a policy the launches do not apply. The load-bearing
// row is the rollback one — ITERION_RELIABILITY_MODE=legacy on a host that
// still carries an enforcing ITERION_EXECUTION_CONTEXT_POLICY.
func TestReliabilityReportNamesThePolicyLaunchesApply(t *testing.T) {
	dir := seedReliabilityStore(t)
	for _, tc := range []struct {
		name  string
		mode  string
		alias string
	}{
		{name: "neither set"},
		{name: "alias alone", alias: "enforce"},
		{name: "rollback over an enforcing alias", mode: "legacy", alias: "enforce"},
		{name: "mode alone", mode: "report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(reliability.EnvMode, tc.mode)
			t.Setenv(reliability.EnvContextPolicyAlias, tc.alias)
			want := reliability.ContextPolicyFromEnv()
			if got := reliabilityJSON(t, ReliabilityOptions{StoreDir: dir}).ContextPolicy; got != string(want) {
				t.Fatalf("reported policy = %s, want %s (what the launch surfaces resolve)", got, want)
			}
		})
	}
}

// TestReliabilityReportSingleRunIsLegacyAware pins that a pre-pilot run
// reads as "legacy", not as a silent success: the rollout doc's whole
// compatibility rule is that a missing field never means green.
func TestReliabilityReportSingleRunIsLegacyAware(t *testing.T) {
	got := reliabilityJSON(t, ReliabilityOptions{StoreDir: seedReliabilityStore(t), RunID: "run-done"})
	if got.Run == nil {
		t.Fatal("no single-run report")
	}
	if got.Run.RunID != "run-done" || !got.Run.LegacyContext || !got.Run.RollbackSafe {
		t.Fatalf("run report = %+v", *got.Run)
	}
	if got.Baseline != nil {
		t.Fatalf("single-run report must not also carry a fleet baseline: %+v", *got.Baseline)
	}
}

func TestReliabilityReportRejectsAnUnknownRun(t *testing.T) {
	err := RunReliabilityReport(
		context.Background(),
		ReliabilityOptions{StoreDir: seedReliabilityStore(t), RunID: "nope"},
		&Printer{W: &bytes.Buffer{}, Format: OutputJSON},
	)
	if err == nil {
		t.Fatal("an unknown run id must not report a clean, empty result")
	}
}

func TestReliabilityReportRejectsAnInaccessibleStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses a 000 directory; the inaccessible case cannot be staged")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/runs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Errorf("restore store permissions: %v", err)
		}
	})

	err := RunReliabilityReport(
		context.Background(),
		ReliabilityOptions{StoreDir: dir},
		&Printer{W: &bytes.Buffer{}, Format: OutputJSON},
	)
	if err == nil {
		t.Fatal("an inaccessible store must not report a successful empty baseline")
	}
}

func TestReliabilityReportNamesAnInaccessibleRunDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses a 000 directory; the inaccessible case cannot be staged")
	}
	dir := seedReliabilityStore(t)
	runDir := dir + "/runs/unreadable-run"
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runDir+"/run.json", []byte(`{"id":"unreadable-run","status":"failed_resumable"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(runDir, 0o755); err != nil {
			t.Errorf("restore run permissions: %v", err)
		}
	})

	got := reliabilityJSON(t, ReliabilityOptions{StoreDir: dir})
	if len(got.Unreadable) != 1 || got.Unreadable[0] != "unreadable-run" {
		t.Fatalf("unreadable runs = %v, want [unreadable-run]", got.Unreadable)
	}
	if got.Baseline == nil || got.Baseline.Total != 2 {
		t.Fatalf("baseline = %+v, want the two readable runs only", got.Baseline)
	}
}

// TestReliabilityRollbackPrintsAnExecutableAction keeps the printed plan
// honest: it must name a variable the rollout actually resolves, so an
// operator following it is not pulling a lever wired to nothing.
func TestReliabilityRollbackPrintsAnExecutableAction(t *testing.T) {
	var buf bytes.Buffer
	if err := RunReliabilityRollback(&Printer{W: &buf, Format: OutputHuman}); err != nil {
		t.Fatalf("RunReliabilityRollback: %v", err)
	}
	if !strings.Contains(buf.String(), reliability.EnvMode+"=legacy") {
		t.Fatalf("rollback output does not tell the operator what to set:\n%s", buf.String())
	}
}

// staticRunSource is a ReliabilityRunSource that is not a store at all.
// It is how the baseline's backend-independence is exercised without a
// live cluster: whatever the operator points at, the same counters are
// computed over its candidates, and a candidate that cannot be loaded is
// NAMED rather than dropped.
type staticRunSource struct {
	name string
	ids  []string
	runs map[string]*store.Run
}

func (s staticRunSource) Describe() string { return s.name }

func (s staticRunSource) CandidateRunIDs(context.Context) ([]string, error) { return s.ids, nil }

func (s staticRunSource) LoadRun(_ context.Context, id string) (*store.Run, error) {
	run, ok := s.runs[id]
	if !ok {
		return nil, fmt.Errorf("no document for %s", id)
	}
	return run, nil
}

// TestReliabilityReportBaselineReadsTheSourceItIsGiven is the seam half of
// slice 1006.1: step 1 of docs/workflow-reliability-1006.md has to be
// runnable where the pilot runs. The pilot runs in the cloud, so a
// baseline that can only resolve a filesystem store next to a working
// directory counts a developer's local runs and compares them with
// nothing.
func TestReliabilityReportBaselineReadsTheSourceItIsGiven(t *testing.T) {
	t.Setenv(reliability.EnvMode, "")
	t.Setenv(reliability.EnvContextPolicyAlias, "")
	src := staticRunSource{
		name: "elsewhere:two-runs-and-an-unreadable-one",
		ids:  []string{"run-done", "run-paused", "run-broken"},
		runs: map[string]*store.Run{
			"run-done":   {ID: "run-done", Status: store.RunStatusFinished},
			"run-paused": {ID: "run-paused", Status: store.RunStatusPausedWaitingHuman},
		},
	}
	got := reliabilityJSON(t, ReliabilityOptions{Source: src})
	if got.Source != src.name {
		t.Fatalf("source = %q, want %q — a baseline that does not name the store it measured cannot be compared with the next one", got.Source, src.name)
	}
	if got.StoreDir != "" {
		t.Fatalf("store_dir = %q, want empty: a non-filesystem source has no store directory, and an empty path reads as the current one", got.StoreDir)
	}
	if got.Baseline == nil {
		t.Fatal("no baseline in the report")
	}
	if got.Baseline.Total != 2 || got.Baseline.Finished != 1 || got.Baseline.Paused != 1 {
		t.Fatalf("baseline = %+v, want the source's two readable runs", *got.Baseline)
	}
	if len(got.Unreadable) != 1 || got.Unreadable[0] != "run-broken" {
		t.Fatalf("unreadable = %v, want [run-broken]: an under-counting baseline is what decides promote-vs-rollback", got.Unreadable)
	}
}

// TestReliabilityReportSingleRunReadsTheSourceItIsGiven pins the per-run
// question on the same seam: --run-id against a cloud store must answer
// about THAT store's document, not about a local directory of the same
// name.
func TestReliabilityReportSingleRunReadsTheSourceItIsGiven(t *testing.T) {
	src := staticRunSource{
		name: "elsewhere:one-run",
		ids:  []string{"run-cloud"},
		runs: map[string]*store.Run{"run-cloud": {ID: "run-cloud", Status: store.RunStatusFinished}},
	}
	got := reliabilityJSON(t, ReliabilityOptions{Source: src, RunID: "run-cloud"})
	if got.Run == nil || got.Run.RunID != "run-cloud" {
		t.Fatalf("run report = %+v, want the source's run", got.Run)
	}
	if got.Baseline != nil {
		t.Fatalf("a single-run report must not also carry a baseline: %+v", *got.Baseline)
	}
}
