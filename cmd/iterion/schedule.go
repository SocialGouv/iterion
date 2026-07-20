package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// scheduleManifest is the shared --manifest override for the whole group.
var scheduleManifest string

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Schedule recurring bot runs via the host crontab",
	Long: `Schedule recurring bot runs without keeping a daemon resident.

A declarative manifest (default ~/.iterion/schedules.yaml) holds the set of
recurring runs. ` + "`schedule install`" + ` materialises it into a managed block of
the host crontab; each cron line calls ` + "`iterion schedule run <name>`" + `, which
re-reads the manifest and executes the run in-process. The host's own cron is
the trigger, so no iterion process needs to stay running.

Subcommands:
  add        Add or update a schedule in the manifest
  list       List schedules
  remove     Remove a schedule from the manifest
  run        Execute one schedule now (what cron invokes)
  audit      Show the tick-decision history (fired / skipped / guard)
  install    Sync the manifest into the host crontab
  uninstall  Remove iterion-managed entries from the host crontab

Example — weekly self-audit (UTC):
  iterion schedule add sec-audit-source-weekly \
    --cron "0 2 * * 1" --bot bots/sec-audit-source/main.bot \
    --workdir "$PWD"
  iterion schedule install        # writes the managed crontab block
`,
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

var scheduleAddOpts cli.ScheduleAddOptions

var scheduleAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add or update a schedule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scheduleAddOpts.Name = args[0]
		scheduleAddOpts.ManifestPath = scheduleManifest
		return cli.RunScheduleAdd(newPrinter(), scheduleAddOpts)
	},
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

var scheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List schedules",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunScheduleList(newPrinter(), cli.ScheduleCommonOptions{ManifestPath: scheduleManifest})
	},
}

// ---------------------------------------------------------------------------
// remove
// ---------------------------------------------------------------------------

var scheduleRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a schedule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunScheduleRemove(newPrinter(), cli.ScheduleRefOptions{
			ScheduleCommonOptions: cli.ScheduleCommonOptions{ManifestPath: scheduleManifest},
			Name:                  args[0],
		})
	},
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

var scheduleRunDryRun bool

var scheduleRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Execute one schedule now (invoked by cron)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunScheduleRun(cmd.Context(), newPrinter(), cli.ScheduleRunOptions{
			ScheduleCommonOptions: cli.ScheduleCommonOptions{ManifestPath: scheduleManifest},
			Name:                  args[0],
			DryRun:                scheduleRunDryRun,
		})
	},
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

var scheduleAuditOpts cli.ScheduleAuditOptions

var scheduleAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Show the tick-decision history (why a schedule fired or was skipped)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		scheduleAuditOpts.ManifestPath = scheduleManifest
		return cli.RunScheduleAudit(newPrinter(), scheduleAuditOpts)
	},
}

// ---------------------------------------------------------------------------
// install / uninstall
// ---------------------------------------------------------------------------

var scheduleInstallOpts cli.ScheduleInstallOptions

var scheduleInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Sync the manifest into the host crontab",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		scheduleInstallOpts.ManifestPath = scheduleManifest
		return cli.RunScheduleInstall(newPrinter(), scheduleInstallOpts)
	},
}

var scheduleUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove iterion-managed entries from the host crontab",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunScheduleUninstall(newPrinter(), cli.ScheduleCommonOptions{ManifestPath: scheduleManifest})
	},
}

func init() {
	scheduleCmd.PersistentFlags().StringVar(&scheduleManifest, "manifest", "", "Schedules manifest path (default: $ITERION_SCHEDULES_FILE or ~/.iterion/schedules.yaml)")

	// add
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Cron, "cron", "", "Cron expression: 5 fields (min hour dom month dow), e.g. \"0 2 * * 1\" (required)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Bot, "bot", "", "Path to the .bot workflow or .botz bundle to run (resolved against --workdir) (required)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Workdir, "workdir", "", "Working directory for the run (default: current directory)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.StoreDir, "store-dir", "", "Store directory passed to the run (default: <workdir>/.iterion)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Sandbox, "sandbox", "", "Sandbox override passed to the run (none|auto)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Timeout, "timeout", "", "Max run duration, e.g. 2h (guards a hung scheduled run)")
	scheduleAddCmd.Flags().StringArrayVar(&scheduleAddOpts.VarFlags, "var", nil, "Workflow variable key=value, merged at run time (repeatable; commas kept verbatim)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Description, "description", "", "Human description (emitted as a crontab comment)")
	scheduleAddCmd.Flags().BoolVar(&scheduleAddOpts.Disabled, "disabled", false, "Keep the entry in the manifest but do not install it into the crontab")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Overlap, "overlap", "", "Overlap policy: skip (default — don't fire while a previous run is live), allow, or keepalive (always-on: relaunch a stale run)")
	scheduleAddCmd.Flags().IntVar(&scheduleAddOpts.MaxConcurrent, "max-concurrent", 0, "With --overlap allow: max live runs including the new one (0 = unlimited)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.StaleAfter, "stale-after", "", "With --overlap keepalive: silence cutoff after which a running run is treated dead and relaunched (Go duration, default 5m)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.Guard, "guard", "", "Pre-launch sh -lc guard: exit 0 fires the run (stdout becomes vars[guard-var]), non-zero skips the tick")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.GuardTimeout, "guard-timeout", "", "Guard subprocess timeout (Go duration, default 30s)")
	scheduleAddCmd.Flags().StringVar(&scheduleAddOpts.GuardVar, "guard-var", "", "Workflow var receiving the guard stdout (default guard_output)")

	// run
	scheduleRunCmd.Flags().BoolVar(&scheduleRunDryRun, "dry-run", false, "Print the resolved `iterion run` command without executing it")

	// audit
	scheduleAuditCmd.Flags().StringVar(&scheduleAuditOpts.Name, "name", "", "Filter to one schedule")
	scheduleAuditCmd.Flags().StringVar(&scheduleAuditOpts.Surface, "surface", "", "Filter by surface (host-cron|trigger|cloud)")
	scheduleAuditCmd.Flags().StringVar(&scheduleAuditOpts.Since, "since", "", "Only records newer than this lookback (Go duration, e.g. 24h)")
	scheduleAuditCmd.Flags().IntVar(&scheduleAuditOpts.Tail, "tail", 50, "Keep only the last N matching records (0 = all)")

	// install
	scheduleInstallCmd.Flags().BoolVar(&scheduleInstallOpts.Print, "print", false, "Render the managed crontab block to stdout without modifying the crontab")
	scheduleInstallCmd.Flags().StringVar(&scheduleInstallOpts.TZ, "tz", "UTC", "Timezone for the schedules (crontab CRON_TZ); honoured by cronie/Vixie cron")

	scheduleCmd.AddCommand(scheduleAddCmd, scheduleListCmd, scheduleRemoveCmd, scheduleRunCmd, scheduleAuditCmd, scheduleInstallCmd, scheduleUninstallCmd)
	rootCmd.AddCommand(scheduleCmd)
}
