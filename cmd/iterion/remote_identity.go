package main

import (
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// remote teams / orgs / me — identity and tenancy management.

var remoteTeamFlag string
var remoteOrgFlag string

// resolveRemoteTeam applies the flag > env/persisted > active-team chain.
func resolveRemoteTeam(cmd *cobra.Command, c *cli.RemoteClient) (string, error) {
	return c.ResolveTeam(cmd.Context(), remoteTeamFlag)
}

func resolveRemoteOrg(cmd *cobra.Command, c *cli.RemoteClient) (string, error) {
	return c.ResolveOrg(cmd.Context(), remoteOrgFlag)
}

var remoteTeamsCmd = &cobra.Command{
	Use:   "teams",
	Short: "Teams on the remote instance",
}

var remoteTeamsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your teams",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteTeamsList(cmd.Context(), c, newPrinter())
	},
}

var remoteTeamsCreateOrg string

var remoteTeamsCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a team",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body := map[string]string{"name": args[0]}
		if remoteTeamsCreateOrg != "" {
			body["org_id"] = remoteTeamsCreateOrg
		}
		p := newPrinter()
		raw, err := c.Call(cmd.Context(), "POST", "/api/teams", body, nil)
		if err != nil {
			return err
		}
		cli.PrintRemoteJSON(p, raw)
		return nil
	},
}

var remoteTeamsSwitchCmd = &cobra.Command{
	Use:   "switch <team-id>",
	Short: "Switch the CLI's default team (mints a team-pinned token)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteTeamsSwitch(cmd.Context(), c, newPrinter(), args[0], cliTokenName())
	},
}

var remoteTeamsMembersCmd = &cobra.Command{
	Use:   "members [set-role <user-id> <role>|remove <user-id>]",
	Short: "List or manage team members",
	Args:  cobra.MaximumNArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		team, err := resolveRemoteTeam(cmd, c)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/teams/" + team + "/members"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "set-role" && len(args) == 3:
			body := fmt.Sprintf(`{"role":%q}`, args[2])
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PATCH", base+"/"+args[1], []byte(body))
		case args[0] == "remove" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: members [set-role <user-id> <role>|remove <user-id>]")
		}
	},
}

var remoteInviteRole string

var remoteTeamsInvitationsCmd = &cobra.Command{
	Use:   "invitations [create <email>|delete <invite-id>]",
	Short: "List or manage team invitations",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		team, err := resolveRemoteTeam(cmd, c)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/teams/" + team + "/invitations"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create" && len(args) == 2:
			role := remoteInviteRole
			if role == "" {
				role = "member"
			}
			body := fmt.Sprintf(`{"email":%q,"role":%q}`, args[1], role)
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, []byte(body))
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: invitations [create <email>|delete <invite-id>]")
		}
	},
}

// --- orgs ---

var remoteOrgsCmd = &cobra.Command{
	Use:   "orgs",
	Short: "Organizations on the remote instance",
}

var remoteOrgsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your orgs",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteOrgsList(cmd.Context(), c, newPrinter())
	},
}

var remoteOrgsSwitchCmd = &cobra.Command{
	Use:   "switch <org-id>",
	Short: "Set the CLI's default org",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteOrgsSwitch(cmd.Context(), c, newPrinter(), args[0])
	},
}

var remoteOrgsMembersCmd = &cobra.Command{
	Use:   "members [set-role <user-id> <role>|remove <user-id>]",
	Short: "List or manage org members",
	Args:  cobra.MaximumNArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		org, err := resolveRemoteOrg(cmd, c)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/orgs/" + org + "/members"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "set-role" && len(args) == 3:
			body := fmt.Sprintf(`{"role":%q}`, args[2])
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PATCH", base+"/"+args[1], []byte(body))
		case args[0] == "remove" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: members [set-role <user-id> <role>|remove <user-id>]")
		}
	},
}

var remoteOrgsInvitationsCmd = &cobra.Command{
	Use:   "invitations [create <email>|delete <invite-id>]",
	Short: "List or manage org invitations",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		org, err := resolveRemoteOrg(cmd, c)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/orgs/" + org + "/invitations"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create" && len(args) == 2:
			role := remoteInviteRole
			if role == "" {
				role = "member"
			}
			body := fmt.Sprintf(`{"email":%q,"role":%q}`, args[1], role)
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, []byte(body))
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: invitations [create <email>|delete <invite-id>]")
		}
	},
}

var remoteOrgsUsageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Org monthly usage (runs, cost, quotas)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		org, err := resolveRemoteOrg(cmd, c)
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/orgs/"+org+"/usage")
	},
}

var remoteOrgsTeamsCmd = &cobra.Command{
	Use:   "teams [create <name>]",
	Short: "List or create teams inside the org",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		org, err := resolveRemoteOrg(cmd, c)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/orgs/" + org + "/teams"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create" && len(args) == 2:
			body := fmt.Sprintf(`{"name":%q}`, args[1])
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, []byte(body))
		default:
			return fmt.Errorf("usage: teams [create <name>]")
		}
	},
}

// --- me ---

var remoteMeCmd = &cobra.Command{
	Use:   "me",
	Short: "Your account on the remote instance",
}

var remoteMePasswordCmd = &cobra.Command{
	Use:   "password",
	Short: "Change your password (prompts; never takes passwords as args)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("password change needs an interactive terminal")
		}
		fmt.Fprint(os.Stderr, "Current password: ")
		oldPw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "New password: ")
		newPw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		body := map[string]string{"current_password": string(oldPw), "new_password": string(newPw)}
		p := newPrinter()
		raw, err := c.Call(cmd.Context(), "POST", "/api/me/password", body, nil)
		if err != nil {
			return err
		}
		if len(raw) > 0 {
			cli.PrintRemoteJSON(p, raw)
		} else {
			p.Line("Password changed")
		}
		return nil
	},
}

var remoteMeSessionsRevokeCmd = &cobra.Command{
	Use:   "sessions-revoke-all",
	Short: "Revoke all your active sessions",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", "/api/me/sessions/revoke-all", nil)
	},
}

var remoteMeSSOLinksCmd = &cobra.Command{
	Use:   "sso-links",
	Short: "List your linked SSO identities",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/me/sso/links")
	},
}

func init() {
	remoteTeamsCreateCmd.Flags().StringVar(&remoteTeamsCreateOrg, "org", "", "Org to create the team in")
	remoteTeamsMembersCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	remoteTeamsInvitationsCmd.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	remoteTeamsInvitationsCmd.Flags().StringVar(&remoteInviteRole, "role", "", "Invitation role (default member)")
	remoteTeamsCmd.AddCommand(remoteTeamsListCmd, remoteTeamsCreateCmd, remoteTeamsSwitchCmd, remoteTeamsMembersCmd, remoteTeamsInvitationsCmd)

	for _, c := range []*cobra.Command{remoteOrgsMembersCmd, remoteOrgsInvitationsCmd, remoteOrgsUsageCmd, remoteOrgsTeamsCmd} {
		c.Flags().StringVar(&remoteOrgFlag, "org", "", "Org id (default: switched/active org)")
	}
	remoteOrgsInvitationsCmd.Flags().StringVar(&remoteInviteRole, "role", "", "Invitation role (default member)")
	remoteOrgsCmd.AddCommand(remoteOrgsListCmd, remoteOrgsSwitchCmd, remoteOrgsMembersCmd, remoteOrgsInvitationsCmd, remoteOrgsUsageCmd, remoteOrgsTeamsCmd)

	remoteMeCmd.AddCommand(remoteMePasswordCmd, remoteMeSessionsRevokeCmd, remoteMeSSOLinksCmd)

	remoteCmd.AddCommand(remoteTeamsCmd, remoteOrgsCmd, remoteMeCmd)
}
