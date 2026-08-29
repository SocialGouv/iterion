package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/modelcatalog"
)

// ModelsOptions configures the `iterion models` command.
type ModelsOptions struct {
	// Spec is an optional "provider/model-id". When empty the command lists a
	// representative set of known models (model.KnownModelSpecs()).
	Spec string
	// Refresh force-refetches the model-spec cache before resolving.
	Refresh bool
}

// RunModels resolves and prints the model catalog: for one model (--spec/arg)
// or a representative known set, it reports the capabilities the runtime would
// resolve, where each value came from (aggregator|curated), and whether this
// host can actually reach the model with the credentials it holds.
//
// It shares pkg/modelcatalog with GET /api/models on purpose — the CLI and the
// studio picker answering "which models can I use here" differently is a bug
// class, not a feature.
func RunModels(ctx context.Context, opts ModelsOptions, p *Printer) error {
	var specs []string
	if s := strings.TrimSpace(opts.Spec); s != "" {
		specs = []string{s}
	}

	cat, err := modelcatalog.Build(ctx, modelcatalog.Options{
		Specs:   specs,
		Refresh: opts.Refresh,
	})
	if err != nil {
		return UserInputError(err)
	}

	if p.Format == OutputJSON {
		p.JSON(cat)
		return nil
	}

	if cat.Refreshed {
		if cat.RefreshError != "" {
			p.Line("! model-spec refresh failed: %s (showing cached/curated values)", cat.RefreshError)
		} else {
			p.Line("✓ model-spec cache refreshed")
		}
		p.Blank()
	}

	p.Header("Model capabilities")
	headers := []string{"MODEL", "SOURCE", "CONTEXT", "REASON", "TOOLS", "TEMP", "PRICE IN/OUT", "USABLE"}
	rows := make([][]string, 0, len(cat.Models))
	for _, m := range cat.Models {
		rows = append(rows, []string{
			m.Spec,
			m.Source,
			formatContextWindow(m.ContextWindow),
			yesNo(m.Reasoning),
			yesNo(m.ToolCall),
			yesNo(m.Temperature),
			formatPrice(m),
			formatUsable(m),
		})
	}
	p.Table(headers, rows)

	// The reasons matter more than the column: "no" without "why" sends the
	// operator hunting through env vars.
	var unusable []modelcatalog.Entry
	for _, m := range cat.Models {
		if !m.Usable {
			unusable = append(unusable, m)
		}
	}
	if len(unusable) > 0 {
		p.Blank()
		p.Line("Not reachable from this host:")
		for _, m := range unusable {
			p.Line("  %s — %s", m.Spec, m.UnusableReason)
		}
	}
	if cat.RecommendedSpec != "" {
		p.Blank()
		p.Line("Recommended on this host: %s", cat.RecommendedSpec)
	}
	return nil
}

// formatUsable renders reachability compactly: the backends that can drive the
// model, or "no" when none can.
func formatUsable(m modelcatalog.Entry) string {
	if m.Reachability == modelcatalog.ReachabilityUnknown {
		return "unknown"
	}
	if !m.Usable {
		return "no"
	}
	return strings.Join(m.Backends, ",")
}

// formatPrice renders the per-million-token rates, and "—" when unknown. A zero
// rate is NOT free — it means no source published one.
func formatPrice(m modelcatalog.Entry) string {
	if !m.PriceKnown {
		return "—"
	}
	return fmt.Sprintf("$%s/$%s", trimPrice(m.InputCostPerM), trimPrice(m.OutputCostPerM))
}

func trimPrice(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// formatContextWindow renders a token count compactly (1M, 200K, 4096) and
// "—" when unknown (zero).
func formatContextWindow(n int) string {
	switch {
	case n <= 0:
		return "—"
	case n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n%1_000 == 0:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
