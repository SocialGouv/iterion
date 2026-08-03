package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/spf13/cobra"
)

// remote pool — lend an LLM subscription to the instance's mutualised
// credential pool, and see what it served.
//
// The terms are the donor's: every ceiling is optional and 0 means "no
// limit on that axis". Nothing here handles the credential itself; that
// was connected once via the OAuth flow and stays sealed server-side.

var remotePoolCmd = &cobra.Command{
	Use:   "pool",
	Short: "Share your LLM subscription with the instance's credential pool",
	Long: "Lend the unused part of your Claude or ChatGPT subscription so runs with no\n" +
		"credential of their own can use it. You set the ceilings, you can see every\n" +
		"run it served, and `pause` stops it taking effect at the next launch.\n\n" +
		"Spend figures are ESTIMATES: a subscription bills nothing per call, so cost is\n" +
		"derived from token counts.",
}

var remotePoolStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show your contributions and what they have given",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolStatus(cmd.Context(), c, p)
	}),
}

var remotePoolHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "List the runs your quota served",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolHistory(cmd.Context(), c, p)
	}),
}

var (
	remotePoolKind       string
	remotePoolUSDPerDay  float64
	remotePoolUSDPerWeek float64
	remotePoolRunsPerDay int
	remotePoolConcurrent int
	remotePoolFromHour   int
	remotePoolToHour     int
	remotePoolBots       []string
)

var remotePoolShareCmd = &cobra.Command{
	Use:   "share",
	Short: "Start (or update) sharing a connected subscription",
	Long: "Every limit is optional; leaving one out means no limit on that axis.\n" +
		"--from-hour/--to-hour restrict sharing to a local time range (19 → 8 shares\n" +
		"overnight); leave both at 0 to share around the clock.",
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		in := cli.PoolPledgeInput{
			Enabled: true,
			Limits: credpool.Limits{
				MaxUSDPerDay:      remotePoolUSDPerDay,
				MaxUSDPerWeek:     remotePoolUSDPerWeek,
				MaxRunsPerDay:     remotePoolRunsPerDay,
				MaxConcurrentRuns: remotePoolConcurrent,
			},
			Bots: remotePoolBots,
		}
		if remotePoolFromHour != 0 || remotePoolToHour != 0 {
			in.Window = &credpool.Window{
				StartHour: remotePoolFromHour,
				EndHour:   remotePoolToHour,
				Timezone:  cli.LocalTimezone(),
			}
		}
		return cli.RemotePoolShare(cmd.Context(), c, p, remotePoolKind, in)
	}),
}

var remotePoolPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Stop serving new runs, keeping your terms for later",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolPause(cmd.Context(), c, p, remotePoolKind, false)
	}),
}

var remotePoolResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume a paused contribution with its previous terms",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolPause(cmd.Context(), c, p, remotePoolKind, true)
	}),
}

var remotePoolWithdrawCmd = &cobra.Command{
	Use:   "withdraw",
	Short: "Remove your contribution entirely",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolWithdraw(cmd.Context(), c, p, remotePoolKind)
	}),
}

var remotePoolDonorsCmd = &cobra.Command{
	Use:   "donors",
	Short: "Operator view: the pool's policy and who is lending to it",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
		if err != nil {
			return err
		}
		return cli.RemotePoolDonors(cmd.Context(), c, p, team)
	}),
}

func init() {
	kindFlagged := []*cobra.Command{
		remotePoolShareCmd, remotePoolPauseCmd, remotePoolResumeCmd, remotePoolWithdrawCmd,
	}
	for _, c := range kindFlagged {
		c.Flags().StringVar(&remotePoolKind, "kind", "claude_code", "Subscription to share (claude_code|codex)")
	}
	remotePoolShareCmd.Flags().Float64Var(&remotePoolUSDPerDay, "max-usd-day", 0, "Estimated spend ceiling per day (0 = no limit)")
	remotePoolShareCmd.Flags().Float64Var(&remotePoolUSDPerWeek, "max-usd-week", 0, "Estimated spend ceiling per week (0 = no limit)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolRunsPerDay, "max-runs-day", 0, "Runs you will serve per day (0 = no limit)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolConcurrent, "max-concurrent", 0, "Runs served at once (0 = no limit)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolFromHour, "from-hour", 0, "Share from this local hour (with --to-hour)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolToHour, "to-hour", 0, "Share until this local hour (exclusive)")
	remotePoolShareCmd.Flags().StringSliceVar(&remotePoolBots, "bots", nil, "Only these bot ids may use it (default: any)")
	remotePoolDonorsCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")

	remotePoolCmd.AddCommand(
		remotePoolStatusCmd, remotePoolHistoryCmd, remotePoolShareCmd,
		remotePoolPauseCmd, remotePoolResumeCmd, remotePoolWithdrawCmd, remotePoolDonorsCmd,
	)
	remoteCmd.AddCommand(remotePoolCmd)
}
