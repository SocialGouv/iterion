package main

import (
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote teams update|status|delete|add-member — the team lifecycle a
// reorg needs: teams used to be create-and-list only, so a naming mistake
// was permanent, Team.Status was read by the launch gate and written by
// nothing, and placing an existing account in a team meant an email
// invitation that account's owner had to accept.

var (
	remoteTeamName   string
	remoteTeamSlug   string
	remoteTeamReason string
	remoteTeamRole   string
)

var remoteTeamsUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Rename a team (--name / --slug)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "")
		if err != nil {
			return err
		}
		body := map[string]any{}
		if cmd.Flags().Changed("name") {
			body["name"] = remoteTeamName
		}
		if cmd.Flags().Changed("slug") {
			body["slug"] = remoteTeamSlug
		}
		if len(body) == 0 {
			return fmt.Errorf("nothing to update: pass --name and/or --slug")
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", base, string(raw), "team JSON")
	}),
}

var remoteTeamsStatusCmd = &cobra.Command{
	Use:   "status <active|suspended|read_only>",
	Short: "Set a team's lifecycle status (org admin)",
	Long: "Writes Team.Status, which the launch gate reads: a suspended or\n" +
		"read-only team keeps its data and its members and launches no run.\n" +
		"Org-admin only — a team resuming itself would undo a governance\n" +
		"action from inside the thing being governed.",
	Args: cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/status")
		if err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"status": args[0], "reason": remoteTeamReason})
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, string(raw), "status JSON")
	}),
}

var remoteTeamsDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete an EMPTY team (org admin)",
	Long: "Refuses a team that still owns repo integrations, forge connections,\n" +
		"api keys or active runs, and names what is left — deleting it would\n" +
		"strand that data under a tenant nothing can reach, and a surviving\n" +
		"webhook would keep firing into it. Tear those down first, or delete\n" +
		"the whole org (`admin orgs delete`), which runs the purge cascade.",
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "")
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base, nil)
	}),
}

var remoteTeamsAddMemberCmd = &cobra.Command{
	Use:   "add-member <user-id>",
	Short: "Place an EXISTING account in the team with a role (--role)",
	Long: "The direct counterpart of an email invitation, for the case the\n" +
		"invitation cannot serve: an account that already exists on this\n" +
		"instance. The user must already be a member of the team's ORG — that\n" +
		"membership is the identity boundary a team grant sits inside.\n" +
		"Idempotent: re-running it sets the role.",
	Args: cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remoteTeamRole == "" {
			return fmt.Errorf("--role is required (viewer|member|admin|owner|config_editor)")
		}
		base, err := teamBase(cmd, c, "/members/"+args[0])
		if err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"role": remoteTeamRole})
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PUT", base, string(raw), "member JSON")
	}),
}

func init() {
	for _, c := range []*cobra.Command{
		remoteTeamsUpdateCmd, remoteTeamsStatusCmd, remoteTeamsDeleteCmd, remoteTeamsAddMemberCmd,
	} {
		c.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	}
	remoteTeamsUpdateCmd.Flags().StringVar(&remoteTeamName, "name", "", "New display name")
	remoteTeamsUpdateCmd.Flags().StringVar(&remoteTeamSlug, "slug", "", "New slug")
	remoteTeamsStatusCmd.Flags().StringVar(&remoteTeamReason, "reason", "", "Reason recorded in the audit log")
	remoteTeamsAddMemberCmd.Flags().StringVar(&remoteTeamRole, "role", "", "Team role (viewer|member|admin|owner|config_editor)")

	remoteTeamsCmd.AddCommand(remoteTeamsUpdateCmd, remoteTeamsStatusCmd, remoteTeamsDeleteCmd, remoteTeamsAddMemberCmd)
}
