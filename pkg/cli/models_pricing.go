package cli

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// The pricing audit exists because two sources of truth for the same number
// were never compared. iterion fetches model specs from the aggregator —
// including cost.input and cost.output — caches them for 24h, and merges them
// into ModelCapabilities. Meanwhile the cost estimator asked a DIFFERENT live
// source and fell back to a hand-maintained table. Nothing ever looked at
// both, so the committed table could sit three times off the published price,
// and a model the aggregator priced could still report no cost at all. Neither
// showed up as an error: a wrong estimate looks exactly like a right one.
//
// The estimator now consults the aggregator itself, as the tier between claw's
// live registry and the committed table, which narrows what this audit can
// find. That is the point — the two remaining verdicts are the two the fix
// cannot close on its own:
//
//   - DISAGREES is now, in practice, "claw's live registry quotes something
//     other than what models.dev publishes". It can no longer mean "the
//     committed table is stale", because a stale entry is reached only when
//     nothing is published to be stale against.
//   - IGNORED narrows to the HALF-published pair: the estimator refuses a pair
//     with only one positive rate (pricing the other half at zero would read as
//     a bargain and be plain wrong), so the price is published and a run still
//     reports none. That is a real gap, and it is the only shape left.
//
// This deliberately does NOT rewrite the table. Prices feed budget decisions,
// the aggregator is itself a third party, and a generator that silently
// applied a 3x change would be a worse failure than the drift it fixes. It
// reports; a human commits.

// PricingRow is one model's committed rate next to the aggregator's.
type PricingRow struct {
	Spec         string  `json:"spec"`
	StaticIn     float64 `json:"static_input_per_m"`
	StaticOut    float64 `json:"static_output_per_m"`
	HasStatic    bool    `json:"has_static"`
	FetchedIn    float64 `json:"fetched_input_per_m"`
	FetchedOut   float64 `json:"fetched_output_per_m"`
	HasFetched   bool    `json:"has_fetched"`
	EffectiveIn  float64 `json:"effective_input_per_m"`
	EffectiveOut float64 `json:"effective_output_per_m"`
	HasEffective bool    `json:"has_effective"`
	Disagrees    bool    `json:"disagrees"`
	OnlyStatic   bool    `json:"only_static"`
	OnlyFetched  bool    `json:"only_fetched"`
}

// PricingResult is the audit's JSON envelope.
type PricingResult struct {
	Refreshed    bool         `json:"refreshed"`
	RefreshError string       `json:"refresh_error,omitempty"`
	Rows         []PricingRow `json:"rows"`
	DriftCount   int          `json:"drift_count"`
}

// PricingOptions configures `iterion models pricing`.
type PricingOptions struct {
	// Refresh force-refetches the aggregator before comparing, so the audit
	// reflects what is published now rather than what a stale cache holds.
	Refresh bool
	// Check makes any disagreement an error, for CI.
	Check bool
}

// RunModelsPricing compares the committed price table against the aggregator's
// published prices and reports every disagreement. Returns an error under
// --check when anything drifted.
func RunModelsPricing(ctx context.Context, opts PricingOptions, p *Printer) error {
	result := PricingResult{}

	if opts.Refresh {
		result.Refreshed = true
		if err := model.RefreshModelSpecs(ctx); err != nil {
			// Non-fatal on purpose: offline, the audit still reports what the
			// committed table holds, which is the number that would be used.
			result.RefreshError = err.Error()
		}
	}

	for _, spec := range auditSpecs() {
		row := PricingRow{Spec: spec}

		bare := spec
		if i := strings.LastIndex(bare, "/"); i >= 0 {
			bare = bare[i+1:]
		}
		row.StaticIn, row.StaticOut, row.HasStatic = cost.StaticRate(bare)
		// What a run is ACTUALLY charged at, after the live registry and then
		// the table. This is the column that matters; the other two explain it.
		row.EffectiveIn, row.EffectiveOut, row.HasEffective = cost.EffectiveRate(spec)

		if rc, err := model.ResolveSpec(spec); err == nil {
			row.FetchedIn, row.FetchedOut = rc.InputCostPerM, rc.OutputCostPerM
			row.HasFetched = row.FetchedIn > 0 || row.FetchedOut > 0
		}

		switch {
		case row.HasEffective && row.HasFetched:
			// Verdicts compare what a run is charged at against what is
			// published — not the table against the aggregator. The table is
			// only one of the three sources the estimator consults, and
			// judging a source the estimator may never reach yields false
			// verdicts. A difference here means claw's live registry answered
			// with a rate other than the published one, since the published
			// pair is what the estimator uses whenever claw has no entry.
			row.Disagrees = !nearlyEqual(row.EffectiveIn, row.FetchedIn) ||
				!nearlyEqual(row.EffectiveOut, row.FetchedOut)
		case row.HasEffective:
			// Priced, but the aggregator has no published rate to check it
			// against — expected for brand-new models.
			row.OnlyStatic = true
		case row.HasFetched:
			// A price is published and a run would still report nothing.
			// Now reachable only for a half-published pair, which the
			// estimator refuses whole rather than pricing one half at zero.
			row.OnlyFetched = true
		}
		if row.Disagrees || row.OnlyFetched {
			result.DriftCount++
		}
		result.Rows = append(result.Rows, row)
	}

	if p != nil && p.Format == OutputJSON {
		p.JSON(result)
	} else {
		printPricingHuman(result, p)
	}

	if opts.Check && result.DriftCount > 0 {
		return fmt.Errorf("pricing drift on %d model(s): the rate a run is charged at disagrees with the published one, or a published price is being ignored (a half-published pair is refused whole). Review each line and update pkg/backend/cost/cost.go deliberately", result.DriftCount)
	}
	return nil
}

// auditSpecs is the union of what the table covers and what the resolver
// lists, so neither side can hide a model by omitting it.
func auditSpecs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range model.KnownModelSpecs() {
		add(s)
	}
	for _, m := range cost.StaticTableModels() {
		if strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") {
			add("openai/" + m)
			continue
		}
		add("anthropic/" + m)
	}
	sort.Strings(out)
	return out
}

// nearlyEqual keeps float representation from reporting drift that is not
// there — a table entry of 15.00 and a fetched 15.0 are the same price.
func nearlyEqual(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func printPricingHuman(r PricingResult, p *Printer) {
	if p == nil {
		return
	}
	if r.RefreshError != "" {
		p.Line("! aggregator refresh failed: %s (comparing against the cached table only)", r.RefreshError)
		p.Blank()
	}
	p.Header("Pricing audit — what a run is charged (claw → aggregator → table) vs what is published")
	headers := []string{"MODEL", "EFFECTIVE in/out", "TABLE in/out", "PUBLISHED in/out", "VERDICT"}
	rows := make([][]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		effective, committed, published, verdict := "—", "—", "—", "ok"
		if row.HasEffective {
			effective = fmt.Sprintf("%.2f / %.2f", row.EffectiveIn, row.EffectiveOut)
		}
		if row.HasStatic {
			committed = fmt.Sprintf("%.2f / %.2f", row.StaticIn, row.StaticOut)
		}
		if row.HasFetched {
			published = fmt.Sprintf("%.2f / %.2f", row.FetchedIn, row.FetchedOut)
		}
		switch {
		case row.Disagrees:
			verdict = "DISAGREES"
		case row.OnlyFetched:
			verdict = "IGNORED — price published (half a pair?), a run reports nothing"
		case row.OnlyStatic:
			verdict = "priced, aggregator has no rate to check it against"
		case !row.HasEffective && !row.HasFetched:
			verdict = "no price anywhere — cost will be omitted"
		}
		rows = append(rows, []string{row.Spec, effective, committed, published, verdict})
	}
	p.Table(headers, rows)
	p.Blank()
	p.Line("%d model(s) need a decision.", r.DriftCount)
	if r.DriftCount > 0 {
		p.Line("Nothing was rewritten: prices feed budget decisions and the aggregator is a third party.")
		p.Line("Edit pkg/backend/cost/cost.go deliberately, then re-run to confirm.")
	}
}
