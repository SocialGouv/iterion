// Package orgusage meters per-org (tenant) monthly run launches and
// LLM spend, and enforces the launch-time caps. It is the billing
// source of truth in cloud mode: Prometheus counters stay global (no
// tenant labels — cardinality discipline), while these Mongo-backed
// counters answer "how much did this org consume this month".
//
// The shape deliberately mirrors pkg/webhooks' Counter (same CAS
// strategy, same month-bucketed document ids) so operators reasoning
// about one quota system can reason about the other.
package orgusage

import (
	"context"
	"math"
	"time"
)

// MonthlyUsage is the read view of one org's current-month counters.
type MonthlyUsage struct {
	// Month is the UTC bucket key, e.g. "2026-06".
	Month string `json:"month"`
	// Runs counts run launches accepted this month (REST + webhook +
	// resume all consume the same budget — a resume re-enters the
	// engine and spends like a launch).
	Runs int `json:"runs"`
	// CostUSD is the metered LLM spend accumulated by runners. Claw
	// (in-process) nodes are metered precisely; delegate backends
	// contribute the figure their CLI reported, or a token estimate
	// when it reports none (a subscription session bills nothing per
	// call). A node whose model the price table cannot price adds
	// nothing — treat this as a floor, not an exact invoice.
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// DenyReason qualifies an AllowRun refusal so the launch gate can map
// it onto the right stable denial token.
type DenyReason string

const (
	// DenyNone — the launch is allowed (and metered).
	DenyNone DenyReason = ""
	// DenyRuns — the monthly run quota is exhausted.
	DenyRuns DenyReason = "runs"
	// DenyCost — the month's accumulated LLM spend has reached the cap.
	DenyCost DenyReason = "cost"
)

// Counter is the per-org monthly metering + enforcement surface.
// Implementations: MongoCounter (production, atomic CAS) and
// MemoryCounter (tests/local). Keep semantics in lock-step.
type Counter interface {
	// AllowRun atomically increments the month's launched-run counter
	// and checks BOTH launch-time caps against the post-increment
	// document in one round trip: maxRuns on the run count and
	// maxCostMillis on the accumulated spend (each 0 = no cap; with no
	// caps the increment still happens — that is the metering). A
	// denied call rolls the increment back and reports which cap hit.
	// The cost check is a soft cap by nature (a run's future spend is
	// unknowable) — in-flight runs finish, new launches are denied.
	AllowRun(ctx context.Context, tenantID string, when time.Time, maxRuns int, maxCostMillis int64) (DenyReason, error)
	// AddSpend accumulates post-hoc LLM cost/token usage for the
	// month. Never gates — AllowRun enforces the cap pre-launch.
	AddSpend(ctx context.Context, tenantID string, when time.Time, costUSD float64, inputTokens, outputTokens int64) error
	// ReleaseRun undoes one AllowRun admission whose launch was
	// ultimately abandoned without any run being created (e.g. the
	// loser of two concurrent duplicate webhook deliveries). Decrements
	// the month's run counter; a missing month document is a no-op.
	ReleaseRun(ctx context.Context, tenantID string, when time.Time) error
	// Usage returns the month's counters for the org. A month with no
	// activity returns the zero value (Month still filled).
	Usage(ctx context.Context, tenantID string, when time.Time) (MonthlyUsage, error)
}

// RetentionDays bounds how long monthly usage documents are retained
// (Mongo TTL). 400 days keeps a full year of history plus margin for
// an annual billing cycle.
const RetentionDays = 400

// monthKey buckets a timestamp into its UTC month.
func monthKey(when time.Time) string { return when.UTC().Format("2006-01") }

// monthStart returns the first instant of the timestamp's UTC month —
// stored on each document so the TTL index can evict old months.
func monthStart(when time.Time) time.Time {
	u := when.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// usageKey is the document id for one org-month.
func usageKey(tenantID string, when time.Time) string {
	return "org|" + tenantID + "|" + monthKey(when)
}

// CostToMillis converts a USD amount to integer thousandths so the
// Mongo $inc stays integral (float $inc would accumulate drift) and
// the launch gate can express Team.MonthlyCostCapUSD in the counter's
// native unit.
func CostToMillis(usd float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(math.Round(usd * 1000))
}

// millisToCost converts back for the read view.
func millisToCost(m int64) float64 { return float64(m) / 1000 }
