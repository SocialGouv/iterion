package asymptote

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

var runCounter int64

func nextRunID(name string) string {
	n := atomic.AddInt64(&runCounter, 1)
	return fmt.Sprintf("test-%s-%d", name, n)
}

// tmpStore returns a filesystem RunStore rooted in t.TempDir() and a cleanup
// callback the caller is expected to defer.
func tmpStore(t *testing.T) store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

// seedRun creates a run, appends a sequence of (loop_iter, judge_output)
// events that simulate `loops` iterations of a review-fix loop, and marks
// the run finished. Returns the runID.
//
// scores is an ordered list of judge-output values to inject. If the value
// is a bool, it's stored under field "approved". If it's a float64, it's
// stored under field "score". This lets a single helper drive both forms.
func seedRun(t *testing.T, s store.RunStore, judgeNode, loopName string, scores []any) string {
	t.Helper()
	ctx := context.Background()
	run, err := s.CreateRun(ctx, nextRunID(judgeNode), "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Iteration 0 has no preceding edge_selected — the first judge run is iter 0.
	_, err = s.AppendEvent(ctx, run.ID, store.Event{
		Type:      store.EventNodeFinished,
		RunID:     run.ID,
		NodeID:    judgeNode,
		Timestamp: time.Now(),
		Data:      judgeData(scores[0]),
	})
	if err != nil {
		t.Fatalf("AppendEvent (iter 0): %v", err)
	}

	for i := 1; i < len(scores); i++ {
		_, err = s.AppendEvent(ctx, run.ID, store.Event{
			Type:      store.EventEdgeSelected,
			RunID:     run.ID,
			Timestamp: time.Now(),
			Data: map[string]any{
				"loop":      loopName,
				"iteration": i,
			},
		})
		if err != nil {
			t.Fatalf("AppendEvent edge (iter %d): %v", i, err)
		}
		_, err = s.AppendEvent(ctx, run.ID, store.Event{
			Type:      store.EventNodeFinished,
			RunID:     run.ID,
			NodeID:    judgeNode,
			Timestamp: time.Now(),
			Data:      judgeData(scores[i]),
		})
		if err != nil {
			t.Fatalf("AppendEvent finished (iter %d): %v", i, err)
		}
	}

	if err := s.UpdateRunStatus(ctx, run.ID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	return run.ID
}

// judgeData builds the EventNodeFinished.Data payload for a value.
func judgeData(v any) map[string]any {
	switch t := v.(type) {
	case bool:
		return map[string]any{"output": map[string]any{"approved": t}}
	case float64:
		return map[string]any{"output": map[string]any{"score": t}}
	case string:
		return map[string]any{"output": map[string]any{"approved": t}}
	default:
		return map[string]any{"output": map[string]any{}}
	}
}

func TestExtractScore(t *testing.T) {
	cases := []struct {
		name  string
		data  map[string]any
		field string
		want  float64
	}{
		{
			name:  "bool true",
			data:  map[string]any{"output": map[string]any{"approved": true}},
			field: "approved",
			want:  1.0,
		},
		{
			name:  "bool false",
			data:  map[string]any{"output": map[string]any{"approved": false}},
			field: "approved",
			want:  0.0,
		},
		{
			name:  "float passthrough",
			data:  map[string]any{"output": map[string]any{"score": 0.7}},
			field: "score",
			want:  0.7,
		},
		{
			name:  "float clamp high",
			data:  map[string]any{"output": map[string]any{"score": 1.5}},
			field: "score",
			want:  1.0,
		},
		{
			name:  "float clamp low",
			data:  map[string]any{"output": map[string]any{"score": -0.2}},
			field: "score",
			want:  0.0,
		},
		{
			name:  "string true",
			data:  map[string]any{"output": map[string]any{"approved": "true"}},
			field: "approved",
			want:  1.0,
		},
		{
			name:  "string numeric",
			data:  map[string]any{"output": map[string]any{"score": "0.42"}},
			field: "score",
			want:  0.42,
		},
		{
			name:  "missing field",
			data:  map[string]any{"output": map[string]any{"other": true}},
			field: "approved",
			want:  0.0,
		},
		{
			name:  "missing output",
			data:  map[string]any{},
			field: "approved",
			want:  0.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := extractScore(tc.data, tc.field)
			if got != tc.want {
				t.Fatalf("extractScore: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseRunBoolApprovals(t *testing.T) {
	s := tmpStore(t)
	scores := []any{false, false, true, true} // 4 iterations, asymptote-style
	runID := seedRun(t, s, "judge", "review", scores)

	got, err := ParseRun(context.Background(), s, runID, ParseOptions{
		JudgeNodeID: "judge",
	})
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	if len(got.Scores) != 4 {
		t.Fatalf("expected 4 scores, got %d", len(got.Scores))
	}
	want := []float64{0, 0, 1, 1}
	for i, s := range got.Scores {
		if s.Score != want[i] {
			t.Errorf("iter %d: score=%v, want %v", i, s.Score, want[i])
		}
		if s.Iteration != i {
			t.Errorf("expected iteration %d at index %d, got %d", i, i, s.Iteration)
		}
	}
	if got.LoopName != "review" {
		t.Errorf("LoopName = %q, want \"review\"", got.LoopName)
	}
}

func TestParseRunNumericScore(t *testing.T) {
	s := tmpStore(t)
	scores := []any{0.3, 0.55, 0.8, 0.95}
	runID := seedRun(t, s, "judge", "review", scores)

	got, err := ParseRun(context.Background(), s, runID, ParseOptions{
		JudgeNodeID:       "judge",
		JudgeField:        "score",
		ApprovalThreshold: 0.6,
	})
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	if got.Scores[0].Approved {
		t.Errorf("iter 0 approved unexpectedly: %.2f", got.Scores[0].Score)
	}
	if !got.Scores[2].Approved {
		t.Errorf("iter 2 not approved: %.2f", got.Scores[2].Score)
	}
}

func TestAggregateGroup(t *testing.T) {
	// Two runs converge at iter 2; one run hits asymptote at iter 1.
	scoresA := []any{false, true, true}
	scoresB := []any{false, false, true}
	scoresC := []any{false, false, false}

	s := tmpStore(t)
	runA := seedRun(t, s, "judge", "review", scoresA)
	runB := seedRun(t, s, "judge", "review", scoresB)
	runC := seedRun(t, s, "judge", "review", scoresC)

	var series []RunSeries
	for _, id := range []string{runA, runB, runC} {
		rs, err := ParseRun(context.Background(), s, id, ParseOptions{JudgeNodeID: "judge"})
		if err != nil {
			t.Fatalf("ParseRun %s: %v", id, err)
		}
		series = append(series, *rs)
	}

	g := AggregateGroup("test", series)
	if g.MaxIter != 2 {
		t.Fatalf("MaxIter = %d, want 2", g.MaxIter)
	}
	if len(g.PerIter) != 3 {
		t.Fatalf("PerIter len = %d, want 3", len(g.PerIter))
	}
	if g.PerIter[0].PassRate != 0.0 {
		t.Errorf("iter 0 pass-rate = %v, want 0.0", g.PerIter[0].PassRate)
	}
	expectedIter1 := 1.0 / 3.0
	if !floatNear(g.PerIter[1].PassRate, expectedIter1, 1e-9) {
		t.Errorf("iter 1 pass-rate = %v, want %v", g.PerIter[1].PassRate, expectedIter1)
	}
	if !floatNear(g.PerIter[2].PassRate, 2.0/3.0, 1e-9) {
		t.Errorf("iter 2 pass-rate = %v, want %v", g.PerIter[2].PassRate, 2.0/3.0)
	}
}

func TestRenderMarkdownContainsKeySections(t *testing.T) {
	s := tmpStore(t)
	runID := seedRun(t, s, "judge", "review", []any{false, true})
	rs, _ := ParseRun(context.Background(), s, runID, ParseOptions{JudgeNodeID: "judge"})

	cmp := Compare(
		AggregateGroup("single-model", []RunSeries{*rs}),
		AggregateGroup("alternated", nil),
	)

	md := RenderMarkdown(cmp, RenderOptions{
		Title:             "Test Bench",
		ApprovalThreshold: 0.5,
		IncludePerRun:     true,
	})

	for _, want := range []string{
		"# Test Bench",
		"## Inputs",
		"## Per-iteration aggregate",
		"## Pass-rate sparkline",
		"## Per-run series",
		"## Reading guide",
		runID,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q", want)
		}
	}
}

func floatNear(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}
