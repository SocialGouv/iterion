// Package benchmark implements a multi-recipe benchmark runner with
// isolated workspaces and comparable metrics collection.
package benchmark

import (
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// RunMetrics holds the aggregated metrics for a single recipe run.
type RunMetrics struct {
	RecipeName   string        `json:"recipe_name"`
	RunID        string        `json:"run_id"`
	Status       string        `json:"status"`
	Verdict      string        `json:"verdict"`
	TotalCostUSD float64       `json:"total_cost_usd"`
	TotalTokens  int           `json:"total_tokens"`
	Iterations   int           `json:"iterations"`
	Duration     time.Duration `json:"duration_ns"`
	DurationStr  string        `json:"duration"`
	Retries      int           `json:"retries"`
	ModelCalls   int           `json:"model_calls"`
}

// CollectMetrics extracts aggregated metrics from a run's persisted events
// and run metadata.
func CollectMetrics(s *store.RunStore, runID, recipeName string, evalPrimary string) (*RunMetrics, error) {
	run, err := s.LoadRun(runID)
	if err != nil {
		return nil, fmt.Errorf("benchmark: load run %s: %w", runID, err)
	}

	events, err := s.LoadEvents(runID)
	if err != nil {
		return nil, fmt.Errorf("benchmark: load events %s: %w", runID, err)
	}

	m := &RunMetrics{
		RecipeName: recipeName,
		RunID:      runID,
		Status:     string(run.Status),
	}

	// Duration.
	if run.FinishedAt != nil {
		m.Duration = run.FinishedAt.Sub(run.CreatedAt)
	} else {
		m.Duration = run.UpdatedAt.Sub(run.CreatedAt)
	}
	m.DurationStr = m.Duration.Round(time.Millisecond).String()

	// Walk events to aggregate.
	modelCallsFromNodes := 0
	for _, evt := range events {
		switch evt.Type {
		case store.EventLLMRequest:
			m.ModelCalls++
		case store.EventLLMRetry:
			m.Retries++
		case store.EventNodeFinished:
			m.Iterations++
			if evt.Data != nil {
				accumulateUsage(m, evt.Data)
				// Count as model call if tokens were consumed (fallback
				// when LLM hooks are not wired, e.g. stub executors).
				if _, hasTokens := evt.Data["_tokens"]; hasTokens && m.ModelCalls == 0 {
					modelCallsFromNodes++
				}
			}
		case store.EventRunFinished, store.EventRunFailed:
			if evt.Data != nil {
				if v, ok := evt.Data["terminal_output"]; ok {
					if tOut, ok := v.(map[string]interface{}); ok {
						accumulateUsage(m, tOut)
					}
				}
			}
		}
	}

	// If no LLM request events were emitted (e.g. stub executor), fall back to
	// counting node_finished events that reported token usage.
	if m.ModelCalls == 0 {
		m.ModelCalls = modelCallsFromNodes
	}

	// Extract verdict from terminal node output if evaluation policy has a primary metric.
	if evalPrimary != "" {
		m.Verdict = extractVerdict(events, evalPrimary)
	}

	return m, nil
}

// accumulateUsage adds _tokens and _cost_usd from a data map to the metrics.
func accumulateUsage(m *RunMetrics, data map[string]interface{}) {
	if v, ok := data["_tokens"]; ok {
		switch t := v.(type) {
		case float64:
			m.TotalTokens += int(t)
		case int:
			m.TotalTokens += t
		}
	}
	if v, ok := data["_cost_usd"]; ok {
		if t, ok := v.(float64); ok {
			m.TotalCostUSD += t
		}
	}
}

// extractVerdict looks through events for the terminal node output and
// extracts the primary metric value.
func extractVerdict(events []*store.Event, primaryMetric string) string {
	// Walk backwards to find the last node_finished before run_finished.
	for i := len(events) - 1; i >= 0; i-- {
		evt := events[i]
		if evt.Type == store.EventNodeFinished && evt.Data != nil {
			if output, ok := evt.Data["output"]; ok {
				if outMap, ok := output.(map[string]interface{}); ok {
					if v, ok := outMap[primaryMetric]; ok {
						return fmt.Sprintf("%v", v)
					}
				}
			}
		}
	}
	return ""
}
