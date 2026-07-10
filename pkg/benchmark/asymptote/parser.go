package asymptote

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ParseRun walks events for a single run and produces a RunSeries scoring
// each iteration of the judge node's enclosing loop.
//
// Heuristic: the iteration counter is read from EventEdgeSelected events
// (data["loop"], data["iteration"]) which the engine emits whenever a
// loop-back edge is traversed. Iteration N's verdict is the EventNodeFinished
// for the JudgeNodeID after the Nth EventEdgeSelected and before the next.
// Iteration 0 (first pass before any loop-back) is the first
// EventNodeFinished for the judge.
func ParseRun(ctx context.Context, s store.RunStore, runID string, opts ParseOptions) (*RunSeries, error) {
	if opts.JudgeNodeID == "" {
		return nil, fmt.Errorf("asymptote.ParseRun: JudgeNodeID is required")
	}
	if opts.JudgeField == "" {
		opts.JudgeField = DefaultJudgeField
	}
	if opts.ApprovalThreshold == 0 {
		opts.ApprovalThreshold = DefaultApprovalThreshold
	}

	run, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("asymptote.ParseRun(%s): load run: %w", runID, err)
	}

	events, err := s.LoadEvents(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("asymptote.ParseRun(%s): load events: %w", runID, err)
	}

	series := &RunSeries{
		RunID:   runID,
		JudgeID: opts.JudgeNodeID,
		Status:  string(run.Status),
	}

	currentIter := 0
	loopName := opts.LoopName

	for _, evt := range events {
		switch evt.Type {
		case store.EventEdgeSelected:
			if evt.Data == nil {
				continue
			}
			lp, _ := evt.Data["loop"].(string)
			if lp == "" {
				continue
			}
			// Anchor on first loop name that touches the judge if not pinned.
			if loopName == "" {
				loopName = lp
			}
			if lp != loopName {
				continue
			}
			currentIter = readIntField(evt.Data, "iteration")

		case store.EventNodeFinished:
			if evt.NodeID != opts.JudgeNodeID {
				continue
			}
			score, raw := extractScore(evt.Data, opts.JudgeField)
			series.Scores = append(series.Scores, IterationScore{
				RunID:     runID,
				LoopName:  loopName,
				Iteration: currentIter,
				Score:     score,
				Approved:  score >= opts.ApprovalThreshold,
				NodeID:    opts.JudgeNodeID,
				RawValue:  raw,
				At:        evt.Timestamp,
			})
		}
	}

	series.LoopName = loopName

	// Stable ordering by iteration; multiple verdicts at the same iter are
	// rare (would only happen if the judge fired twice without a loopback)
	// — preserve original order in that case.
	sort.SliceStable(series.Scores, func(i, j int) bool {
		return series.Scores[i].Iteration < series.Scores[j].Iteration
	})

	return series, nil
}

// readIntField pulls an int value out of an event's Data map, tolerating
// the float64 form JSON unmarshal produces.
func readIntField(data map[string]any, key string) int {
	v, ok := data[key]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n
		}
	}
	return 0
}

// extractScore reads the configured judge field from an EventNodeFinished
// data payload and maps it to a 0..1 score. Booleans become 1.0/0.0,
// numeric values pass through (clamped 0..1), strings are parsed for
// "true"/"false"/numeric values, and missing values produce 0.0.
//
// Returns (score, raw-string-for-debug).
func extractScore(data map[string]any, field string) (float64, string) {
	if data == nil {
		return 0, ""
	}
	output, ok := data["output"].(map[string]any)
	if !ok {
		return 0, ""
	}
	v, ok := output[field]
	if !ok {
		return 0, ""
	}
	raw := fmt.Sprintf("%v", v)
	switch t := v.(type) {
	case bool:
		if t {
			return 1.0, raw
		}
		return 0.0, raw
	case float64:
		return clamp01(t), raw
	case int:
		return clamp01(float64(t)), raw
	case int64:
		return clamp01(float64(t)), raw
	case string:
		if t == "true" {
			return 1.0, raw
		}
		if t == "false" {
			return 0.0, raw
		}
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return clamp01(f), raw
		}
	}
	return 0, raw
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
