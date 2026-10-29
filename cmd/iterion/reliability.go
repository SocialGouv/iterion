package main

import (
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/spf13/cobra"
)

var reliabilityCmd = &cobra.Command{
	Use:   "reliability",
	Short: "Inspect and roll back the workflow-reliability rollout",
	Long: `Read-only operator surface for the staged workflow-reliability rollout
(docs/workflow-reliability-1006.md).

Subcommands:
  report     Resolved rollout mode + a fleet baseline, or one run's compatibility report
  rollback   Print the non-destructive rollback plan
`,
}

var reliabilityReportOpts struct {
	storeDir   string
	runID      string
	cloud      bool
	tenantID   string
	configPath string
}

var reliabilityReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Show the resolved rollout mode and a pilot baseline",
	Long: `Print the rollout mode the launch surfaces actually apply for this
environment (ITERION_RELIABILITY_MODE, falling back to the older
ITERION_EXECUTION_CONTEXT_POLICY), the retry-circuit settings, and either a
fleet baseline over the store's runs or one run's compatibility report.

This is step 1 of the pilot procedure — capture it BEFORE switching modes and
again after, and compare. Nothing here writes a run: a report never upgrades a
legacy run's policy.

Which store it reads matters, because the pilot runs in the cloud: without
--cloud the baseline covers the FILESYSTEM store the working directory
resolves to (a developer's local runs), which is not the population a cloud
promotion is decided on. --cloud reads the Mongo run store instead, through
the same configuration the server and runner use (ITERION_MONGO_URI plus
ITERION_S3_*, which the cloud store requires to open at all), and --tenant
scopes it to one tenant's runs. The report's "store" line always names the
source it actually read.

Examples:
  iterion reliability report                       # fleet baseline, local store
  iterion reliability report --cloud --json        # the baseline a promotion is decided on
  iterion reliability report --cloud --tenant t1   # one tenant's cloud runs
  iterion reliability report --run-id <id>         # one run's compatibility
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.ReliabilityOptions{
			StoreDir: reliabilityReportOpts.storeDir,
			RunID:    reliabilityReportOpts.runID,
		}
		if reliabilityReportOpts.cloud {
			if reliabilityReportOpts.storeDir != "" {
				return cli.UserInputError(errors.New("reliability report: --cloud and --store-dir name two different run stores; pass one"))
			}
			src, closeSrc, err := cloudReliabilitySource(cmd, reliabilityReportOpts.configPath, reliabilityReportOpts.tenantID)
			if err != nil {
				return err
			}
			defer closeSrc()
			opts.Source = src
		} else if reliabilityReportOpts.tenantID != "" {
			return cli.UserInputError(errors.New("reliability report: --tenant scopes a cloud baseline and needs --cloud; the filesystem store is not tenant-partitioned"))
		}
		return cli.RunReliabilityReport(cmd.Context(), opts, newPrinter())
	},
}

// cloudReliabilitySource opens the Mongo run store for a read-only
// baseline, through the SAME wire-up every cloud subcommand uses
// (newCloudBlob / newCloudMongoStore): one configuration path, so a
// baseline cannot measure a different cluster than the one the pilot runs
// on.
//
// The blob client is built because the cloud store refuses to open
// without one (artifact bodies live in S3); the baseline reads no
// artifact body. Opening the store also ensures its schema, exactly as
// every other cloud process does — index maintenance, which writes no
// run: the report's read-only promise is about run documents.
//
// The returned closer releases both and is the caller's to defer.
func cloudReliabilitySource(cmd *cobra.Command, configPath, tenantID string) (cli.ReliabilityRunSource, func(), error) {
	ctx := cmd.Context()
	cfg, err := iterconfig.Load(iterconfig.LoadOptions{
		YAMLPath:         configPath,
		DefaultLogFormat: iterconfig.LogFormatHuman,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reliability report: load config: %w", err)
	}
	if cfg.Mongo.URI == "" || cfg.S3.Bucket == "" {
		return nil, nil, cli.UserInputError(errors.New(
			"reliability report: --cloud requires ITERION_MONGO_URI and ITERION_S3_BUCKET (or a --config naming them) — the cloud run store does not open without the blob client, even for a read-only baseline"))
	}
	bc, err := newCloudBlob(ctx, cfg.S3)
	if err != nil {
		return nil, nil, fmt.Errorf("reliability report: build blob client: %w", err)
	}
	ms, err := newCloudMongoStore(ctx, cfg.Mongo, bc, cfg.Log.NewLogger(cmd.ErrOrStderr()), nil)
	if err != nil {
		_ = bc.Close()
		return nil, nil, fmt.Errorf("reliability report: open cloud run store: %w", err)
	}
	return cli.NewMongoReliabilitySource(ms, cfg.Mongo.DB, tenantID), func() {
		closeCloudStoreWithTimeout(ms)
		_ = bc.Close()
	}, nil
}

var reliabilityRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Print the non-destructive rollback plan",
	Long: `Print the rollback contract: which variable to set, and what evidence to
keep. It prints — it does not mutate. The variables are read where the launch
surfaces run (a pod spec, a service unit, a shell), which a one-shot CLI
process cannot reach.
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunReliabilityRollback(newPrinter())
	},
}

func init() {
	f := reliabilityReportCmd.Flags()
	f.StringVar(&reliabilityReportOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the working directory)")
	f.StringVar(&reliabilityReportOpts.runID, "run-id", "", "Report on a single run instead of the fleet baseline")
	f.BoolVar(&reliabilityReportOpts.cloud, "cloud", false, "Read the Mongo run store the cloud runs on (requires ITERION_MONGO_URI and ITERION_S3_*) instead of the local filesystem store")
	f.StringVar(&reliabilityReportOpts.tenantID, "tenant", "", "Scope a --cloud baseline to one tenant's runs (default: every tenant)")
	f.StringVar(&reliabilityReportOpts.configPath, "config", "", "YAML configuration naming the cloud store (--cloud only)")

	reliabilityCmd.AddCommand(reliabilityReportCmd)
	reliabilityCmd.AddCommand(reliabilityRollbackCmd)
	rootCmd.AddCommand(reliabilityCmd)
}
