package main

import (
	"errors"
	"os"
	"testing"
)

// TestOrgLimitDefaultsFromEnv_TheForkAuthorBoundShipsAPerimeter: the fork
// review lane's per-contributor bound (ADR-162 Q4), read through the ONE
// function the server boot calls. Unset ships 20 runs / $20 — a perimeter,
// never "open"; the literal `unbounded` is the one spelling of no cap; a
// positive number is the cap; and every other value — 0 above all, which the
// neighbouring ITERION_ORG_DEFAULT_* knobs read as "no limit" — refuses the
// boot instead of opening the perimeter or being silently replaced.
//
// Mutations: the shipped default 20 → 0 (the unset row reds); drop the
// `unbounded` arm (the hatch rows red); accept n == 0 (the "0 runs" row
// reds); drop the dollar knob's CostCapMillis refusal (the "0 dollars",
// sub-millidollar, vanishing, inf, past-int64 and NaN rows red).
func TestOrgLimitDefaultsFromEnv_TheForkAuthorBoundShipsAPerimeter(t *testing.T) {
	for _, row := range []struct {
		name     string
		runs     string
		usd      string
		set      bool
		wantRuns int
		wantUSD  float64
		refused  bool
	}{
		{name: "unset ships the perimeter", wantRuns: 20, wantUSD: 20},
		{name: "the literal unbounded is the hatch", set: true, runs: "unbounded", usd: "unbounded", wantRuns: 0, wantUSD: 0},
		{name: "the hatch is case-insensitive and trimmed", set: true, runs: " UNBOUNDED ", usd: "Unbounded", wantRuns: 0, wantUSD: 0},
		{name: "a positive cap", set: true, runs: "7", usd: "2.5", wantRuns: 7, wantUSD: 2.5},
		{name: "0 runs is refused, not a second unbounded", set: true, runs: "0", usd: "20", refused: true},
		{name: "0 dollars is refused, not a second unbounded", set: true, runs: "20", usd: "0", refused: true},
		{name: "a negative cap is refused", set: true, runs: "-1", usd: "20", refused: true},
		{name: "a typo is refused, never silently replaced by the default", set: true, runs: "twenty", usd: "20", refused: true},
		// The dollar knob's values that parse as positive (or as no number
		// at all) yet reach AllowRun as "no cap" through CostToMillis: each is
		// an implicit unbounded, refused like 0.
		{name: "a sub-millidollar cap is refused, it rounds to no cap", set: true, runs: "20", usd: "0.0004", refused: true},
		{name: "a vanishing cap is refused", set: true, runs: "20", usd: "1e-9", refused: true},
		{name: "inf is refused", set: true, runs: "20", usd: "inf", refused: true},
		{name: "a cap past int64 millidollars is refused", set: true, runs: "20", usd: "1e300", refused: true},
		{name: "NaN is refused", set: true, runs: "20", usd: "NaN", refused: true},
		{name: "one millidollar is a cap", set: true, runs: "20", usd: "0.001", wantRuns: 20, wantUSD: 0.001},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv(forkAuthorRunQuotaEnv, row.runs)
			t.Setenv(forkAuthorCostCapEnv, row.usd)
			if !row.set {
				// A real unset (t.Setenv's cleanup restores the original):
				// the operator who never wrote the knob.
				if err := os.Unsetenv(forkAuthorRunQuotaEnv); err != nil {
					t.Fatal(err)
				}
				if err := os.Unsetenv(forkAuthorCostCapEnv); err != nil {
					t.Fatal(err)
				}
			}
			d, err := orgLimitDefaultsFromEnv()
			if row.refused {
				if !errors.Is(err, errForkBoundEnv) {
					t.Fatalf("orgLimitDefaultsFromEnv() = %+v, %v — want the boot refused (errForkBoundEnv)", d, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("orgLimitDefaultsFromEnv: %v", err)
			}
			if d.ForkAuthorRunQuota != row.wantRuns || d.ForkAuthorCostCapUSD != row.wantUSD {
				t.Fatalf("fork-author bound = %d runs / $%v, want %d / $%v", d.ForkAuthorRunQuota, d.ForkAuthorCostCapUSD, row.wantRuns, row.wantUSD)
			}
		})
	}
}
