package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Run iterion benchmarks against persisted runs",
	Long:  `Bench produces empirical reports from existing runs in the store. Subcommands target specific theses (e.g. asymptote).`,
}

var benchAsymptoteOpts struct {
	storeDir          string
	runs              string
	variantRuns       string
	label             string
	variantLabel      string
	judgeNode         string
	judgeField        string
	loopName          string
	approvalThreshold float64
	output            string
	title             string
	includePerRun     bool
}

var benchAsymptoteCmd = &cobra.Command{
	Use:   "asymptote",
	Short: "Measure inter-session quality stabilisation curves",
	Long: `Asymptote computes per-iteration judge verdicts across a set of
runs of the same workflow and renders a stabilisation report (markdown
or JSON). Optionally compare against a recipe variant (e.g. multi-family
alternation for security-critical tasks).

The thesis (see docs/why-iterion.md) holds that running the same task in
N independent sessions of the same workflow produces a quality curve
that climbs and stabilises — that stabilisation is the asymptote, and
it measures the (model + recipe)'s reliability ceiling for the task.
Multi-family alternation can raise that ceiling for critical work, but
it is not the default — pass it as a --variant-runs comparison.

Examples:
  # Canonical: stabilisation curve over N sessions of the same workflow.
  iterion bench asymptote \
      --runs r1,r2,r3,r4,r5 \
      --judge-node final_judge \
      --output docs/asymptote-bench-2026-05.md

  # Compare against a multi-family variant for a security-critical task.
  iterion bench asymptote \
      --runs r1,r2,r3 --label single-family \
      --variant-runs r4,r5,r6 --variant-label multi-family \
      --judge-node final_judge \
      --output report.md

  # Numeric judge field with a non-default approval threshold.
  iterion bench asymptote \
      --runs r1 \
      --judge-node review_judge --judge-field score --approval-threshold 0.8 \
      --include-per-run --output -`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.BenchAsymptoteOptions{
			StoreDir:          benchAsymptoteOpts.storeDir,
			Runs:              cli.SplitRunIDs(benchAsymptoteOpts.runs),
			VariantRuns:       cli.SplitRunIDs(benchAsymptoteOpts.variantRuns),
			Label:             benchAsymptoteOpts.label,
			VariantLabel:      benchAsymptoteOpts.variantLabel,
			JudgeNode:         benchAsymptoteOpts.judgeNode,
			JudgeField:        benchAsymptoteOpts.judgeField,
			LoopName:          benchAsymptoteOpts.loopName,
			ApprovalThreshold: benchAsymptoteOpts.approvalThreshold,
			Output:            benchAsymptoteOpts.output,
			Title:             benchAsymptoteOpts.title,
			IncludePerRun:     benchAsymptoteOpts.includePerRun,
		}
		return cli.RunBenchAsymptote(opts, newPrinter())
	},
}

func init() {
	f := benchAsymptoteCmd.Flags()
	f.StringVar(&benchAsymptoteOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the working directory)")
	f.StringVar(&benchAsymptoteOpts.runs, "runs", "", "Comma-separated run IDs of the same workflow (the canonical asymptote subjects)")
	f.StringVar(&benchAsymptoteOpts.variantRuns, "variant-runs", "", "Comma-separated run IDs of an alternative recipe variant (optional)")
	f.StringVar(&benchAsymptoteOpts.label, "label", "", "Primary group label (default: asymptote)")
	f.StringVar(&benchAsymptoteOpts.variantLabel, "variant-label", "", "Variant group label (default: variant)")
	f.StringVar(&benchAsymptoteOpts.judgeNode, "judge-node", "", "IR node ID of the judge whose verdicts will be scored (required)")
	f.StringVar(&benchAsymptoteOpts.judgeField, "judge-field", "", "Output field on the judge node carrying the verdict (default: approved)")
	f.StringVar(&benchAsymptoteOpts.loopName, "loop", "", "Restrict scoring to one bounded loop name (default: first observed)")
	f.Float64Var(&benchAsymptoteOpts.approvalThreshold, "approval-threshold", 0, "Score threshold for the approved flag (default: 0.5)")
	f.StringVar(&benchAsymptoteOpts.output, "output", "", "Markdown output file (- or empty for stdout)")
	f.StringVar(&benchAsymptoteOpts.title, "title", "", "Report title (default: Asymptote Benchmark)")
	f.BoolVar(&benchAsymptoteOpts.includePerRun, "include-per-run", false, "Append a per-run iteration list at the end")
	mustMarkRequired(benchAsymptoteCmd, "judge-node")

	benchCmd.AddCommand(benchAsymptoteCmd)
	rootCmd.AddCommand(benchCmd)
}
