// Package server — runs stats aggregation surface.
//
// The studio's existing /runs view shows one run at a time; nothing
// aggregates across runs. Operators couldn't answer "is iterion
// getting more expensive over time?" or "which bot fails most?"
// without grepping events.jsonl by hand. This file exposes a JSON
// aggregation under /api/v1/runs/stats that the studio's /insights
// view consumes.
//
// Scope (deliberately narrow for first iteration):
//   - cost-per-day, faceted by workflow_name
//   - status counts per workflow (finished / failed / cancelled / etc.)
//   - duration P50/P95 per workflow
//   - top-line totals
//
// Cost is not persisted in run.json today; it lives as `_cost_usd`
// on node_finished event payloads. The aggregator walks events.jsonl
// per run via ScanEvents — bounded by `since` (default 30 days) so a
// long-lived store doesn't pay for every historical run on each
// dashboard load. Sub-second on hundreds of runs in practice; if a
// store grows past low thousands of recent runs we'll add a server-
// side memoization keyed by (runID, updated_at).

package server

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// CostBucket is one row in the daily cost time series.
type CostBucket struct {
	// Day is the UTC date label ("YYYY-MM-DD"). Used as the X-axis
	// tick on the studio chart.
	Day string `json:"day"`
	// CostByWorkflow maps workflow_name → USD spent that day.
	// Workflows with zero cost are omitted to keep the payload small.
	CostByWorkflow map[string]float64 `json:"cost_by_workflow"`
	// Total is the day's aggregate across all workflows.
	Total float64 `json:"total"`
}

// WorkflowStats is the per-workflow row consumed by the duration +
// fail-rate panels.
type WorkflowStats struct {
	Workflow   string `json:"workflow"`
	RunCount   int    `json:"run_count"`
	FailCount  int    `json:"fail_count"`
	FailRate   float64 `json:"fail_rate"`     // failed / runCount, 0..1
	DurationP50Sec float64 `json:"duration_p50_sec"` // 0 when no finished runs
	DurationP95Sec float64 `json:"duration_p95_sec"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	// CountsByStatus exposes the raw histogram so the studio can
	// render a stacked breakdown if it wants.
	CountsByStatus map[string]int `json:"counts_by_status"`
}

// StatsResponse is the JSON shape returned by GET /api/v1/runs/stats.
type StatsResponse struct {
	// SinceDays mirrors the request window so the studio can label
	// the dashboard ("last 30 days").
	SinceDays    int           `json:"since_days"`
	TotalRuns    int           `json:"total_runs"`
	TotalCostUSD float64       `json:"total_cost_usd"`
	CostByDay    []CostBucket  `json:"cost_by_day"`
	Workflows    []WorkflowStats `json:"workflows"`
}

func (s *Server) registerRunsStatsRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.HandleFunc("GET /api/v1/runs/stats", s.handleRunsStats)
}

func (s *Server) handleRunsStats(w http.ResponseWriter, r *http.Request) {
	if s.runs == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, errNoRunStore)
		return
	}
	sinceDays := 30
	if v := r.URL.Query().Get("since_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			sinceDays = n
		}
	}
	since := time.Now().UTC().AddDate(0, 0, -sinceDays)

	runs, err := s.runs.ListCtx(r.Context(), runview.ListFilter{Since: since})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err)
		return
	}

	out := aggregateRunStats(r.Context(), s.runs, runs, sinceDays)
	writeJSONResp(w, http.StatusOK, out)
}

// aggregateRunStats turns a slice of run summaries into the
// StatsResponse the studio consumes. Pulled out of the HTTP handler
// so it can be unit-tested without spinning a server.
func aggregateRunStats(
	ctx context.Context,
	svc *runview.Service,
	runs []runview.RunSummary,
	sinceDays int,
) StatsResponse {
	// Pass 1: index runs by day + workflow, accumulate counts, derive
	// duration / cost per run from events.jsonl (one ScanEvents call
	// per run).
	type wfAcc struct {
		runs           int
		fail           int
		statusCounts   map[string]int
		durations      []float64 // seconds, for finished/failed/cancelled
		totalCostUSD   float64
	}
	wfs := map[string]*wfAcc{}
	days := map[string]map[string]float64{} // day → workflow → cost
	totalCost := 0.0

	for i := range runs {
		r := &runs[i]
		wf := r.WorkflowName
		if wf == "" {
			wf = "(unnamed)"
		}
		acc, ok := wfs[wf]
		if !ok {
			acc = &wfAcc{statusCounts: map[string]int{}}
			wfs[wf] = acc
		}
		acc.runs++
		statusStr := string(r.Status)
		acc.statusCounts[statusStr]++
		if isFailStatus(r.Status) {
			acc.fail++
		}
		if dur := finishedDuration(r); dur > 0 {
			acc.durations = append(acc.durations, dur)
		}

		cost := sumRunCost(ctx, svc, r.ID)
		acc.totalCostUSD += cost
		totalCost += cost
		if cost > 0 {
			day := r.CreatedAt.UTC().Format("2006-01-02")
			if _, ok := days[day]; !ok {
				days[day] = map[string]float64{}
			}
			days[day][wf] += cost
		}
	}

	// Materialise CostByDay sorted oldest → newest so the studio can
	// render a left-to-right line chart without resorting.
	costByDay := make([]CostBucket, 0, len(days))
	for day, byWf := range days {
		total := 0.0
		for _, c := range byWf {
			total += c
		}
		costByDay = append(costByDay, CostBucket{
			Day: day, CostByWorkflow: byWf, Total: total,
		})
	}
	sort.Slice(costByDay, func(i, j int) bool { return costByDay[i].Day < costByDay[j].Day })

	// Materialise Workflows sorted by run count desc, then name asc.
	wfList := make([]WorkflowStats, 0, len(wfs))
	for name, a := range wfs {
		p50, p95 := percentiles(a.durations)
		rate := 0.0
		if a.runs > 0 {
			rate = float64(a.fail) / float64(a.runs)
		}
		wfList = append(wfList, WorkflowStats{
			Workflow:       name,
			RunCount:       a.runs,
			FailCount:      a.fail,
			FailRate:       rate,
			DurationP50Sec: p50,
			DurationP95Sec: p95,
			TotalCostUSD:   a.totalCostUSD,
			CountsByStatus: a.statusCounts,
		})
	}
	sort.Slice(wfList, func(i, j int) bool {
		if wfList[i].RunCount != wfList[j].RunCount {
			return wfList[i].RunCount > wfList[j].RunCount
		}
		return wfList[i].Workflow < wfList[j].Workflow
	})

	return StatsResponse{
		SinceDays:    sinceDays,
		TotalRuns:    len(runs),
		TotalCostUSD: totalCost,
		CostByDay:    costByDay,
		Workflows:    wfList,
	}
}

// isFailStatus collapses the run-store's failure-shaped statuses into
// one bucket for the fail-rate metric. failed_resumable counts because
// the operator's experience is "this run did not finish on its own"
// even though it might still resume later.
func isFailStatus(s store.RunStatus) bool {
	switch s {
	case store.RunStatusFailed, store.RunStatusFailedResumable:
		return true
	}
	return false
}

// finishedDuration returns the run's wall-clock duration in seconds
// for runs that reached a terminal state; 0 for still-running /
// paused / queued.
func finishedDuration(r *runview.RunSummary) float64 {
	switch r.Status {
	case store.RunStatusFinished,
		store.RunStatusFailed,
		store.RunStatusFailedResumable,
		store.RunStatusCancelled:
		end := r.UpdatedAt
		if r.FinishedAt != nil && !r.FinishedAt.IsZero() {
			end = *r.FinishedAt
		}
		if r.CreatedAt.IsZero() || end.Before(r.CreatedAt) {
			return 0
		}
		return end.Sub(r.CreatedAt).Seconds()
	}
	return 0
}

// sumRunCost walks the run's events.jsonl via ScanEvents and totals
// every node_finished event's `_cost_usd` payload field. Returns 0
// when the file is missing or the events stream errors — the
// dashboard treats missing-cost as "free", which is the truthful
// reading (no LLM calls = no cost).
func sumRunCost(ctx context.Context, svc *runview.Service, runID string) float64 {
	rs := svc.RunStore()
	if rs == nil {
		return 0
	}
	total := 0.0
	_ = rs.ScanEvents(ctx, runID, func(e *store.Event) bool {
		if e.Type != store.EventNodeFinished {
			return true
		}
		if e.Data == nil {
			return true
		}
		if v, ok := e.Data["_cost_usd"]; ok {
			if f, ok := v.(float64); ok && f > 0 {
				total += f
			}
		}
		return true
	})
	return total
}

// percentiles returns the P50 and P95 of the given sample (in seconds
// for our use). Returns 0,0 on an empty input. Sorts in place to
// avoid an extra allocation; callers don't reuse the slice anyway.
func percentiles(samples []float64) (p50, p95 float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	sort.Float64s(samples)
	pick := func(p float64) float64 {
		// Linear-interpolated percentile — close enough for a
		// dashboard sample, and avoids the off-by-one of the simple
		// idx = floor(p*N) form on small N.
		if len(samples) == 1 {
			return samples[0]
		}
		idx := p * float64(len(samples)-1)
		lo := int(idx)
		hi := lo + 1
		if hi >= len(samples) {
			return samples[len(samples)-1]
		}
		frac := idx - float64(lo)
		return samples[lo]*(1-frac) + samples[hi]*frac
	}
	return pick(0.50), pick(0.95)
}

// errNoRunStore is the sentinel returned to clients when the server
// was started without a run store (cloud control plane, dispatcher-
// only mode).
var errNoRunStore = statsErrString("no run store configured on this server")

type statsErrString string

func (e statsErrString) Error() string { return string(e) }
