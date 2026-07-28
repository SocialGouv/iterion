package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var modelsOpts struct {
	refresh bool
}

var modelsCmd = &cobra.Command{
	Use:   "models [provider/model-id]",
	Short: "Inspect resolved model capabilities",
	Long: `Show the ModelCapabilities iterion resolves for a model — context
window plus reasoning / tool-call / temperature support — and where each value
came from: the online aggregator (models.dev, cached under ~/.iterion) or the
curated static fallback table.

With no argument, a representative set of known models is listed. Pass an
explicit "provider/model-id" to resolve a single model. Use --refresh to
force-refetch the model-spec cache before resolving.

Examples:
  iterion models                                  # list known models
  iterion models anthropic/glm-5.2                # resolve one model
  iterion models openai/gpt-5.5 --json            # machine-readable
  iterion models --refresh                        # refresh cache, then list`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.ModelsOptions{Refresh: modelsOpts.refresh}
		if len(args) == 1 {
			opts.Spec = args[0]
		}
		return cli.RunModels(cmd.Context(), opts, newPrinter())
	},
}

var modelsPricingOpts struct {
	refresh bool
	check   bool
}

// The audit answers a question nothing could answer before: iterion fetches
// published prices AND keeps a committed table, and the two were never held
// side by side. A stale table does not fail — it reports a confident wrong
// number — and a model the aggregator prices can still report no cost at all.
var modelsPricingCmd = &cobra.Command{
	Use:   "pricing",
	Short: "Compare the committed price table against published prices",
	Long: `Hold iterion's committed cost table next to the prices published by the
spec aggregator (models.dev), and report every disagreement.

Nothing is rewritten. Prices feed budget decisions and the aggregator is a
third party, so a change is a judgement call: the audit reports, a human edits
pkg/backend/cost/cost.go and commits.

Three verdicts matter:
  DISAGREES   the committed rate and the published rate differ
  IGNORED     a price is published and the estimator still reports nothing
  table only  the aggregator has no price — expected for brand-new models,
              which is exactly why the committed table exists

Examples:
  iterion models pricing                  # audit against the cached specs
  iterion models pricing --refresh        # refetch first, then audit
  iterion models pricing --check          # non-zero exit on drift, for CI`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunModelsPricing(cmd.Context(), cli.PricingOptions{
			Refresh: modelsPricingOpts.refresh,
			Check:   modelsPricingOpts.check,
		}, newPrinter())
	},
}

func init() {
	modelsCmd.Flags().BoolVar(&modelsOpts.refresh, "refresh", false,
		"Force-refetch the model-spec cache before resolving")
	modelsPricingCmd.Flags().BoolVar(&modelsPricingOpts.refresh, "refresh", false,
		"Force-refetch published prices before comparing")
	modelsPricingCmd.Flags().BoolVar(&modelsPricingOpts.check, "check", false,
		"Exit non-zero when the committed table disagrees with published prices")
	modelsCmd.AddCommand(modelsPricingCmd)
	rootCmd.AddCommand(modelsCmd)
}
