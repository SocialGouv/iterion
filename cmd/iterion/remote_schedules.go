package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote schedules — team-scoped CRUD over cloud recurring bot schedules.
// Cloud-only (list/create/delete 404 when the target instance has no cloud
// scheduler wired). Reuses teamBase + the shared JSON printers.

var remoteSchedulesCmd = &cobra.Command{
	Use:   "schedules",
	Short: "Cron recurring bot schedules (team scope, cloud only)",
}

var remoteSchedulesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List schedules for the active team",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/schedules")
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, base)
	}),
}

var remoteSchedulesCreateData string

var remoteSchedulesCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a schedule (--data JSON: {bot_id, cron, vars?, repo_url?, repo_ref?, disabled?})",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/schedules")
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteSchedulesCreateData, "schedule JSON")
	}),
}

var remoteSchedulesDeleteCmd = &cobra.Command{
	Use:   "delete <schedule-id>",
	Short: "Delete a schedule",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/schedules")
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[0], nil)
	}),
}

func init() {
	remoteSchedulesCreateCmd.Flags().StringVar(&remoteSchedulesCreateData, "data", "", "Schedule JSON (literal, @file, or @- for stdin)")
	for _, c := range []*cobra.Command{remoteSchedulesListCmd, remoteSchedulesCreateCmd, remoteSchedulesDeleteCmd} {
		c.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	}
	remoteSchedulesCmd.AddCommand(remoteSchedulesListCmd, remoteSchedulesCreateCmd, remoteSchedulesDeleteCmd)
	remoteCmd.AddCommand(remoteSchedulesCmd)
}
