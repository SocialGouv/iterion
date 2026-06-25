package quality

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// approx reports whether a and b are within 1e-9 (float-precision slack).
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// newSnap builds a Snapshot whose aggregate carries the two headline
// scores (plus a third dimension) so Compare/IsRegression have something
// to diff.
func newSnap(name, runID string, at time.Time, overall, value float64) *Snapshot {
	return &Snapshot{
		Kind:  "bot",
		Name:  name,
		RunID: runID,
		At:    at,
		Aggregate: Aggregate{
			MeanScores: map[Dimension]float64{
				DimOverall:       overall,
				DimValueForMoney: value,
				DimEfficacy:      overall,
			},
		},
	}
}

func TestSnapshotStore_RoundTripAndOrdering(t *testing.T) {
	dir := t.TempDir()
	st := NewSnapshotStore(dir)

	base := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	// Write three snapshots out of order; List must return chronological.
	snaps := []*Snapshot{
		newSnap("review-pr", "run-c", base.Add(2*time.Hour), 0.80, 0.70),
		newSnap("review-pr", "run-a", base, 0.60, 0.50),
		newSnap("review-pr", "run-b", base.Add(1*time.Hour), 0.70, 0.60),
	}
	for _, s := range snaps {
		if _, err := st.Write(s); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	paths, err := st.List("review-pr")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("expected 3 history files, got %d", len(paths))
	}

	last, ok, err := st.Last("review-pr")
	if err != nil || !ok {
		t.Fatalf("Last: ok=%v err=%v", ok, err)
	}
	if last.RunID != "run-c" {
		t.Errorf("Last should be the chronologically newest (run-c), got %s", last.RunID)
	}
	if got := last.Overall(); got != 0.80 {
		t.Errorf("Last.Overall = %v, want 0.80", got)
	}
}

func TestSnapshotStore_Write_Idempotent(t *testing.T) {
	st := NewSnapshotStore(t.TempDir())
	s := newSnap("nexie", "run-x", time.Date(2026, 6, 25, 9, 30, 0, 0, time.UTC), 0.5, 0.5)
	p1, err := st.Write(s)
	if err != nil {
		t.Fatalf("Write #1: %v", err)
	}
	p2, err := st.Write(s)
	if err != nil {
		t.Fatalf("Write #2: %v", err)
	}
	if p1 != p2 {
		t.Errorf("same snapshot must map to same path: %q vs %q", p1, p2)
	}
	paths, _ := st.List("nexie")
	if len(paths) != 1 {
		t.Errorf("idempotent write should leave one file, got %d", len(paths))
	}
}

func TestSnapshotStore_Last_NoHistory(t *testing.T) {
	st := NewSnapshotStore(t.TempDir())
	_, ok, err := st.Last("never-run")
	if err != nil {
		t.Fatalf("Last on empty: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false for a target with no history")
	}
}

func TestCompare_NilBaseline(t *testing.T) {
	cur := newSnap("x", "r1", time.Now().UTC(), 0.9, 0.9)
	if d := Compare(nil, cur); d != nil {
		t.Errorf("Compare with nil prev must be nil, got %+v", d)
	}
}

func TestCompare_Verdicts(t *testing.T) {
	at := time.Date(2026, 6, 25, 8, 0, 0, 0, time.UTC)
	prev := newSnap("x", "p", at, 0.60, 0.60)
	cases := []struct {
		name    string
		overall float64
		want    Relative
	}{
		{"clearly better", 0.80, RelBetter},
		{"clearly worse", 0.40, RelWorse},
		{"within band same", 0.62, RelSame},
		{"edge below band", 0.60 - DefaultSameBand + 0.001, RelSame},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := newSnap("x", "c", at.Add(time.Hour), tc.overall, 0.60)
			d := Compare(prev, cur)
			if d == nil {
				t.Fatal("expected non-nil delta")
			}
			if d.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q (overallDelta=%.3f)", d.Verdict, tc.want, d.OverallDelta)
			}
		})
	}
}

func TestDelta_IsRegression(t *testing.T) {
	at := time.Now().UTC()
	prev := newSnap("x", "p", at, 0.80, 0.80)

	// A drop beyond tolerance on overall → regression.
	cur := newSnap("x", "c", at.Add(time.Hour), 0.80-DefaultRegressTolerance-0.02, 0.80)
	d := Compare(prev, cur)
	reg, reasons := d.IsRegression(0)
	if !reg {
		t.Errorf("expected regression, got none (delta=%.3f)", d.OverallDelta)
	}
	if len(reasons) == 0 {
		t.Errorf("regression must carry reasons")
	}

	// A small dip within tolerance → no regression.
	cur2 := newSnap("x", "c2", at.Add(2*time.Hour), 0.80-0.02, 0.80)
	if reg, _ := Compare(prev, cur2).IsRegression(0); reg {
		t.Errorf("small dip should not be a regression")
	}

	// Nil delta → never a regression.
	if reg, _ := (*Delta)(nil).IsRegression(0); reg {
		t.Errorf("nil delta must not be a regression")
	}

	// Value-for-money drop alone trips the gate too.
	cur3 := newSnap("x", "c3", at.Add(3*time.Hour), 0.80, 0.80-DefaultRegressTolerance-0.05)
	if reg, _ := Compare(prev, cur3).IsRegression(0); !reg {
		t.Errorf("value_for_money drop should be a regression")
	}
}

func TestAggregateScores_MeanAndDisagreement(t *testing.T) {
	verdicts := []JudgeVerdict{
		{Scores: map[Dimension]float64{DimOverall: 0.6, DimEfficacy: 0.8}},
		{Scores: map[Dimension]float64{DimOverall: 0.8, DimEfficacy: 0.8}},
	}
	mean, spread := aggregateScores(verdicts)
	if got := mean[DimOverall]; !approx(got, 0.7) {
		t.Errorf("mean overall = %v, want ~0.7", got)
	}
	if got := spread[DimOverall]; !approx(got, 0.2) {
		t.Errorf("spread overall = %v, want ~0.2 (0.8-0.6)", got)
	}
	if got := spread[DimEfficacy]; !approx(got, 0) {
		t.Errorf("spread efficacy = %v, want ~0 (agreement)", got)
	}
}

func TestToVerdict_ClampAndFamilyFlag(t *testing.T) {
	raw := judgeRaw{
		Overall:         1.5,  // clamps to 1.0
		Efficacy:        -0.3, // clamps to 0.0
		ValueForMoney:   0.5,
		Narrative:       "ok",
		RelativeOverall: "BETTER", // normalises to better
		Confidence:      2.0,      // clamps to 1.0
	}
	v := toVerdict("anthropic/claude-sonnet-4-6", "anthropic", raw)
	if v.Scores[DimOverall] != 1.0 {
		t.Errorf("overall should clamp to 1.0, got %v", v.Scores[DimOverall])
	}
	if v.Scores[DimEfficacy] != 0.0 {
		t.Errorf("efficacy should clamp to 0.0, got %v", v.Scores[DimEfficacy])
	}
	if v.Confidence != 1.0 {
		t.Errorf("confidence should clamp to 1.0, got %v", v.Confidence)
	}
	if !v.SameFamilyAsBot {
		t.Errorf("expected SameFamilyAsBot=true for anthropic judge on anthropic bot")
	}
	if v.RelativeVsPrev[DimOverall] != RelBetter {
		t.Errorf("relative overall should normalise BETTER→better")
	}
}

func TestJudgeSchemaJSON_Valid(t *testing.T) {
	raw := judgeSchemaJSON()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("judge schema is not valid JSON: %v", err)
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	// Flat schema: every score is a top-level property (no nested "scores").
	for _, want := range []string{"efficacy", "overall", "value_for_money", "narrative", "confidence"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema missing top-level property %q", want)
		}
	}
	req, ok := m["required"].([]any)
	if !ok || len(req) == 0 {
		t.Errorf("schema missing required list")
	}
}

func TestDefaultJudgeModels_EnvOverride(t *testing.T) {
	t.Setenv("ITERION_LIVE_JUDGE_MODELS", "openai/gpt-5-mini, anthropic/claude-haiku-4-5 ")
	got := DefaultJudgeModels()
	want := []string{"openai/gpt-5-mini", "anthropic/claude-haiku-4-5"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("env override = %v, want %v", got, want)
	}
}

func TestDefaultJudgeModels_CrossFamilyDefault(t *testing.T) {
	t.Setenv("ITERION_LIVE_JUDGE_MODELS", "")
	got := DefaultJudgeModels()
	if len(got) != 2 {
		t.Fatalf("default panel should be 2 models, got %v", got)
	}
	if family(got[0]) == family(got[1]) {
		t.Errorf("default panel must be cross-family, got %v", got)
	}
}

func TestRenderEvidence_IncludesArtifactAndMetrics(t *testing.T) {
	ev := Evidence{
		Kind:        "bot",
		Name:        "review-pr",
		Task:        "find the planted bug",
		WorkProduct: "DIFF: +func Foo()",
		Outcome:     "status=finished",
		Metrics:     Metrics{CostUSD: 0.12, Tokens: 3456, DurationMS: 9000},
	}
	out := renderEvidence(ev, nil)
	for _, want := range []string{"review-pr", "find the planted bug", "DIFF: +func Foo()", "cost_usd: 0.1200", "first snapshot"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered evidence missing %q\n---\n%s", want, out)
		}
	}
}
