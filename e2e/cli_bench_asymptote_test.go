package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// `iterion bench asymptote` is the empirical readout of the convergence
// thesis: point it at N runs of the same bot and it reconstructs, per loop
// iteration, how many runs were still iterating and how many the judge had
// approved. It re-runs nothing — the whole measurement is derived from what
// the runs persisted — so the honest e2e is: drive real runs through the CLI
// with a stub executor, then benchmark THOSE runs and check the curve matches
// the verdicts the judge actually produced.
//
// Mutation check: cut the wire between the run's event stream and the parser
// (drop the judge's node_finished payload, or the loop's iteration index) and
// the per-iteration counts/pass-rates below stop matching; drop the variant
// group and the two-group comparison collapses.

// benchRunFixture drives asymptote_mini.bot for as many refine iterations as
// approveAt implies: the judge disapproves until its approveAt-th verdict
// (1-based), which is what makes each run's curve distinct.
func benchRunFixture(t *testing.T, storeDir, runID string, approveAt int) {
	t.Helper()
	verdicts := 0
	exec := newScenarioExecutor()
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"content": "draft"}, nil
	})
	exec.on("review", func(_ map[string]any) (map[string]any, error) {
		verdicts++
		return map[string]any{
			"approved": verdicts >= approveAt,
			"notes":    "fixture verdict",
		}, nil
	})
	if err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "asymptote_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Executor:      exec,
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON}); err != nil {
		t.Fatalf("run %s: %v", runID, err)
	}
}

// benchJSON runs the benchmark in machine mode and decodes the comparison.
func benchJSON(t *testing.T, opts cli.BenchAsymptoteOptions) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := cli.RunBenchAsymptote(opts, &cli.Printer{W: &buf, Format: cli.OutputJSON}); err != nil {
		t.Fatalf("bench asymptote: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode comparison (%q): %v", buf.String(), err)
	}
	return out
}

// perIter pulls one iteration aggregate out of a decoded group.
func perIter(t *testing.T, cmp map[string]any, group string, iter int) map[string]any {
	t.Helper()
	g, ok := cmp[group].(map[string]any)
	if !ok {
		t.Fatalf("comparison has no %q group: %v", group, cmp)
	}
	rows, ok := g["per_iter"].([]any)
	if !ok || iter >= len(rows) {
		t.Fatalf("group %q has no iteration %d (per_iter=%v)", group, iter, g["per_iter"])
	}
	row, ok := rows[iter].(map[string]any)
	if !ok {
		t.Fatalf("group %q iteration %d is not an object: %v", group, iter, rows[iter])
	}
	return row
}

func num(t *testing.T, row map[string]any, key string) float64 {
	t.Helper()
	v, ok := row[key].(float64)
	if !ok {
		t.Fatalf("field %q missing or not numeric in %v", key, row)
	}
	return v
}

func TestBenchAsymptoteMeasuresTheConvergenceCurve(t *testing.T) {
	storeDir := t.TempDir()
	// Three runs of the same bot converging at different speeds: the judge
	// approves on its 3rd, 2nd and 3rd verdict. Everyone is disapproved at
	// iteration 0; half the still-iterating population passes at the next.
	benchRunFixture(t, storeDir, "bench-slow", 3)
	benchRunFixture(t, storeDir, "bench-fast", 2)
	benchRunFixture(t, storeDir, "bench-slow-2", 3)

	cmp := benchJSON(t, cli.BenchAsymptoteOptions{
		StoreDir:  storeDir,
		Runs:      []string{"bench-slow", "bench-fast", "bench-slow-2"},
		JudgeNode: "review",
		Label:     "primary",
	})

	if got := cmp["max_iter"]; got != float64(2) {
		t.Errorf("max_iter = %v, want 2 (the slowest run needed three verdicts)", got)
	}
	if got := cmp["single"].(map[string]any)["label"]; got != "primary" {
		t.Errorf("group label = %v, want the --label value", got)
	}

	// Iteration 0: all three runs produced a verdict, none approved.
	first := perIter(t, cmp, "single", 0)
	if got := num(t, first, "count"); got != 3 {
		t.Errorf("iteration 0 count = %v, want 3 — a run's first verdict never reached the benchmark", got)
	}
	if got := num(t, first, "pass_rate"); got != 0 {
		t.Errorf("iteration 0 pass_rate = %v, want 0", got)
	}
	if got := num(t, first, "mean_score"); got != 0 {
		t.Errorf("iteration 0 mean_score = %v, want 0", got)
	}

	// Iteration 1: still three runs, one of them (bench-fast) approved.
	second := perIter(t, cmp, "single", 1)
	if got := num(t, second, "count"); got != 3 {
		t.Errorf("iteration 1 count = %v, want 3", got)
	}
	if got := num(t, second, "pass_rate"); got < 0.33 || got > 0.34 {
		t.Errorf("iteration 1 pass_rate = %v, want ~1/3 (only the fast run had converged)", got)
	}

	// Iteration 2: only the two slow runs are still iterating, and both pass.
	// A converged run is EXCLUDED here rather than padded with a zero — that
	// is what keeps the curve from bending back down.
	third := perIter(t, cmp, "single", 2)
	if got := num(t, third, "count"); got != 2 {
		t.Errorf("iteration 2 count = %v, want 2 — the converged run was padded into the tail", got)
	}
	if got := num(t, third, "pass_rate"); got != 1 {
		t.Errorf("iteration 2 pass_rate = %v, want 1", got)
	}
}

func TestBenchAsymptoteComparesTwoGroups(t *testing.T) {
	storeDir := t.TempDir()
	benchRunFixture(t, storeDir, "bench-base", 3)
	benchRunFixture(t, storeDir, "bench-variant", 1)

	cmp := benchJSON(t, cli.BenchAsymptoteOptions{
		StoreDir:     storeDir,
		Runs:         []string{"bench-base"},
		VariantRuns:  []string{"bench-variant"},
		JudgeNode:    "review",
		Label:        "baseline",
		VariantLabel: "alternated",
	})

	if got := cmp["single"].(map[string]any)["label"]; got != "baseline" {
		t.Errorf("primary label = %v, want baseline", got)
	}
	if got := cmp["alternated"].(map[string]any)["label"]; got != "alternated" {
		t.Errorf("variant label = %v, want alternated", got)
	}
	// The variant converged on its very first verdict, the baseline did not:
	// the two groups must not be reading the same series.
	if got := num(t, perIter(t, cmp, "alternated", 0), "pass_rate"); got != 1 {
		t.Errorf("variant iteration 0 pass_rate = %v, want 1", got)
	}
	if got := num(t, perIter(t, cmp, "single", 0), "pass_rate"); got != 0 {
		t.Errorf("baseline iteration 0 pass_rate = %v, want 0", got)
	}
	if got := cmp["alternated"].(map[string]any)["max_iter"]; got != float64(0) {
		t.Errorf("variant max_iter = %v, want 0 (it approved immediately)", got)
	}
}

func TestBenchAsymptoteWritesTheMarkdownReport(t *testing.T) {
	storeDir := t.TempDir()
	benchRunFixture(t, storeDir, "bench-md", 2)

	out := filepath.Join(t.TempDir(), "asymptote.md")
	if err := cli.RunBenchAsymptote(cli.BenchAsymptoteOptions{
		StoreDir:      storeDir,
		Runs:          []string{"bench-md"},
		JudgeNode:     "review",
		Label:         "primary",
		Title:         "Fixture asymptote",
		Output:        out,
		IncludePerRun: true,
	}, &cli.Printer{W: io.Discard, Format: cli.OutputHuman}); err != nil {
		t.Fatalf("bench asymptote: %v", err)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	md := string(body)
	for _, want := range []string{"Fixture asymptote", "primary", "bench-md"} {
		if !strings.Contains(md, want) {
			t.Errorf("report does not mention %q:\n%s", want, md)
		}
	}
}

func TestBenchAsymptoteRefusesAnIncompleteRequest(t *testing.T) {
	storeDir := t.TempDir()
	benchRunFixture(t, storeDir, "bench-guard", 1)

	t.Run("no judge node", func(t *testing.T) {
		err := cli.RunBenchAsymptote(cli.BenchAsymptoteOptions{
			StoreDir: storeDir,
			Runs:     []string{"bench-guard"},
		}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
		if err == nil || !strings.Contains(err.Error(), "judge-node") {
			t.Fatalf("err = %v, want a --judge-node requirement", err)
		}
	})

	t.Run("no runs", func(t *testing.T) {
		err := cli.RunBenchAsymptote(cli.BenchAsymptoteOptions{
			StoreDir:  storeDir,
			JudgeNode: "review",
		}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
		if err == nil || !strings.Contains(err.Error(), "--runs") {
			t.Fatalf("err = %v, want a missing-subjects error", err)
		}
	})

	t.Run("an unknown run is named, not silently dropped", func(t *testing.T) {
		err := cli.RunBenchAsymptote(cli.BenchAsymptoteOptions{
			StoreDir:  storeDir,
			Runs:      []string{"bench-guard", "bench-absent"},
			JudgeNode: "review",
		}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
		if err == nil || !strings.Contains(err.Error(), "bench-absent") {
			t.Fatalf("err = %v, want the missing run id in the message", err)
		}
	})
}
