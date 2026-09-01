package main

import (
	"fmt"
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
	Short: "Share an LLM subscription or API key with the instance's credential pool",
	Long: "Lend the unused part of your Claude or ChatGPT subscription — or a personal\n" +
		"API key of any provider — so runs with no credential of their own can use it.\n" +
		"You set the ceilings, you can see every run it served, and `pause` stops it\n" +
		"taking effect at the next launch.\n\n" +
		"For a SUBSCRIPTION the spend figures are ESTIMATES: the provider bills nothing\n" +
		"per call, so cost is derived from token counts. For an API KEY they are actual\n" +
		"charges on your own invoice — which is why a spend ceiling is required there.",
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
	remotePoolSource     string
	remotePoolRef        string
	remotePoolKeyID      string
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
			Bots:  remotePoolBots,
			KeyID: remotePoolKeyID,
		}
		if remotePoolFromHour != 0 || remotePoolToHour != 0 {
			in.Window = &credpool.Window{
				StartHour: remotePoolFromHour,
				EndHour:   remotePoolToHour,
				Timezone:  cli.LocalTimezone(),
			}
		}
		return cli.RemotePoolShare(cmd.Context(), c, p, remotePoolSource, remotePoolRef, in)
	}),
}

var remotePoolPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Stop serving new runs, keeping your terms for later",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolPause(cmd.Context(), c, p, remotePoolSource, remotePoolRef, false)
	}),
}

var remotePoolResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume a paused contribution with its previous terms",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolPause(cmd.Context(), c, p, remotePoolSource, remotePoolRef, true)
	}),
}

var remotePoolWithdrawCmd = &cobra.Command{
	Use:   "withdraw",
	Short: "Remove your contribution entirely",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemotePoolWithdraw(cmd.Context(), c, p, remotePoolSource, remotePoolRef)
	}),
}

var (
	remotePoolName            string
	remotePoolEnabled         bool
	remotePoolAudAllTeams     bool
	remotePoolAudContributors bool
	remotePoolAudOrgs         []string
	remotePoolAudTeams        []string
)

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

// remote pool policy — the OPERATOR half. `donors` shows the pool; this
// creates or changes it. Until now the only way to open a pool was a raw
// `remote api PUT`, which meant hand-writing the audience JSON: the one
// step of standing a pool up had no command.
var remotePoolPolicyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Operator: create or update the pool (name, master switch, who may draw on it)",
	Long: `Create or update the credential pool of this team's ORG.

The pool document is keyed by org, so its policy governs every team under
that org — the server treats changing it as an org-level decision and
refuses a team admin who is not an org admin.

Audience (pick what fits; they combine):
  --all-teams          every team on the instance
  --orgs a,b           every team under these orgs (the pool's own org is
                       always admitted)
  --teams x,y          exactly these teams
  --contributors       anyone who is themselves an active donor ("lend to
                       borrow")

Only the flags you set are sent, so ` + "`pool policy --enabled=false`" + ` pauses
a pool without restating its audience. The AUDIENCE, though, is replaced
whole: naming any one audience flag clears the three you did not name, so
restate every dial you mean to keep.

A pool that does not exist yet is created OPEN — pass --enabled=false to
write a policy while keeping it paused.`,
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
		if err != nil {
			return err
		}
		var pol cli.PoolPolicy
		if cmd.Flags().Changed("name") {
			pol.Name = &remotePoolName
		}
		if cmd.Flags().Changed("enabled") {
			pol.Enabled = &remotePoolEnabled
		}
		// The audience is sent WHOLE or not at all: it is a set, and
		// merging a partial one server-side would let `--teams x` silently
		// keep a forgotten --all-teams from a previous call.
		if cmd.Flags().Changed("all-teams") || cmd.Flags().Changed("orgs") ||
			cmd.Flags().Changed("teams") || cmd.Flags().Changed("contributors") {
			pol.Audience = &struct {
				Teams        []string `json:"teams,omitempty"`
				Orgs         []string `json:"orgs,omitempty"`
				Contributors bool     `json:"contributors,omitempty"`
				AllTeams     bool     `json:"all_teams,omitempty"`
			}{
				Teams:        remotePoolAudTeams,
				Orgs:         remotePoolAudOrgs,
				Contributors: remotePoolAudContributors,
				AllTeams:     remotePoolAudAllTeams,
			}
		}
		if pol.Name == nil && pol.Enabled == nil && pol.Audience == nil {
			return fmt.Errorf("nothing to change — set --name, --enabled, or an audience flag (--all-teams/--orgs/--teams/--contributors)")
		}
		return cli.RemotePoolPolicy(cmd.Context(), c, p, team, pol)
	}),
}

func init() {
	kindFlagged := []*cobra.Command{
		remotePoolShareCmd, remotePoolPauseCmd, remotePoolResumeCmd, remotePoolWithdrawCmd,
	}
	for _, c := range kindFlagged {
		c.Flags().StringVar(&remotePoolSource, "source", "oauth", "What you lend: oauth (a subscription) or api_key (a metered provider key)")
		c.Flags().StringVar(&remotePoolRef, "ref", "claude_code", "Which one: a subscription (claude_code|codex) or a provider (anthropic, openai, …)")
	}
	remotePoolShareCmd.Flags().StringVar(&remotePoolKeyID, "key-id", "", "Which of your keys to lend (required for --source api_key)")
	remotePoolShareCmd.Flags().Float64Var(&remotePoolUSDPerDay, "max-usd-day", 0, "Estimated spend ceiling per day (0 = no limit)")
	remotePoolShareCmd.Flags().Float64Var(&remotePoolUSDPerWeek, "max-usd-week", 0, "Estimated spend ceiling per week (0 = no limit)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolRunsPerDay, "max-runs-day", 0, "Runs you will serve per day (0 = no limit)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolConcurrent, "max-concurrent", 0, "Runs served at once (0 = no limit)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolFromHour, "from-hour", 0, "Share from this local hour (with --to-hour)")
	remotePoolShareCmd.Flags().IntVar(&remotePoolToHour, "to-hour", 0, "Share until this local hour (exclusive)")
	remotePoolShareCmd.Flags().StringSliceVar(&remotePoolBots, "bots", nil, "Only these bot ids may use it (default: any)")
	remotePoolDonorsCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")

	remotePoolPolicyCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	remotePoolPolicyCmd.Flags().StringVar(&remotePoolName, "name", "", "Pool name")
	remotePoolPolicyCmd.Flags().BoolVar(&remotePoolEnabled, "enabled", true, "Master switch: off skips the pool tier entirely (a pool being created comes up on)")
	remotePoolPolicyCmd.Flags().BoolVar(&remotePoolAudAllTeams, "all-teams", false, "Audience: every team on the instance")
	remotePoolPolicyCmd.Flags().StringSliceVar(&remotePoolAudOrgs, "orgs", nil, "Audience: every team under these org ids")
	remotePoolPolicyCmd.Flags().StringSliceVar(&remotePoolAudTeams, "teams", nil, "Audience: exactly these team ids")
	remotePoolPolicyCmd.Flags().BoolVar(&remotePoolAudContributors, "contributors", false, "Audience: anyone who is themselves an active donor")

	remotePoolCmd.AddCommand(
		remotePoolStatusCmd, remotePoolHistoryCmd, remotePoolShareCmd,
		remotePoolPauseCmd, remotePoolResumeCmd, remotePoolWithdrawCmd, remotePoolDonorsCmd,
		remotePoolPolicyCmd,
	)
	remoteCmd.AddCommand(remotePoolCmd)
}
