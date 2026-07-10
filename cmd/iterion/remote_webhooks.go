package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote webhooks / forge — inbound-webhook management and forge
// integrations, both team-scoped.

var remoteWebhooksData string

var remoteWebhooksCmd = &cobra.Command{
	Use:   "webhooks",
	Short: "Inbound webhook endpoints (team scope)",
}

func remoteWebhooksBase(cmd *cobra.Command, c *cli.RemoteClient) (string, error) {
	team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
	if err != nil {
		return "", err
	}
	return "/api/teams/" + team + "/webhooks", nil
}

var remoteWebhooksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List webhooks",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		base, err := remoteWebhooksBase(cmd, c)
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), base)
	},
}

var remoteWebhooksGetCmd = &cobra.Command{
	Use:   "get <webhook-id>",
	Short: "Show a webhook",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		base, err := remoteWebhooksBase(cmd, c)
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), base+"/"+args[0])
	},
}

var remoteWebhooksCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a webhook (--data @file)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		base, err := remoteWebhooksBase(cmd, c)
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteWebhooksData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required (webhook config JSON)")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", base, body)
	},
}

var remoteWebhooksUpdateCmd = &cobra.Command{
	Use:   "update <webhook-id>",
	Short: "Update a webhook (--data @file)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		base, err := remoteWebhooksBase(cmd, c)
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteWebhooksData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "PATCH", base+"/"+args[0], body)
	},
}

var remoteWebhooksDeleteCmd = &cobra.Command{
	Use:   "delete <webhook-id>",
	Short: "Delete a webhook",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		base, err := remoteWebhooksBase(cmd, c)
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "DELETE", base+"/"+args[0], nil)
	},
}

var remoteWebhooksRotateCmd = &cobra.Command{
	Use:   "rotate <webhook-id>",
	Short: "Rotate the webhook's token",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		base, err := remoteWebhooksBase(cmd, c)
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", base+"/"+args[0]+"/rotate", nil)
	},
}

var remoteWebhooksDeliveriesCmd = &cobra.Command{
	Use:   "deliveries <webhook-id>",
	Short: "List the webhook's recent deliveries",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		base, err := remoteWebhooksBase(cmd, c)
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), base+"/"+args[0]+"/deliveries")
	},
}

// --- forge ---

var remoteForgeData string

var remoteForgeCmd = &cobra.Command{
	Use:   "forge",
	Short: "Forge connections and provisioning (team scope)",
}

var remoteForgeConnectionsCmd = &cobra.Command{
	Use:   "connections [create|delete <conn-id>|repos <conn-id>]",
	Short: "Forge connections",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/teams/" + team + "/forge/connections"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			body, err := cli.ReadDataArg(remoteForgeData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required (connection JSON)")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, body)
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		case args[0] == "repos" && len(args) == 2:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/"+args[1]+"/repos")
		default:
			return fmt.Errorf("usage: connections [create --data @f|delete <id>|repos <id>]")
		}
	},
}

var remoteForgeRepoBotsCmd = &cobra.Command{
	Use:   "repo-bots [create|preview|delete <integration-id>]",
	Short: "Repo↔bot provisioning (forge integrations)",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/teams/" + team + "/forge/repo-bots"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "preview":
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/preview")
		case args[0] == "create":
			body, err := cli.ReadDataArg(remoteForgeData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, body)
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: repo-bots [create --data @f|preview|delete <id>]")
		}
	},
}

var remoteForgeOAuthAppsCmd = &cobra.Command{
	Use:   "oauth-apps [create|delete <app-id>]",
	Short: "Per-tenant forge OAuth apps",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/teams/" + team + "/forge/oauth-apps"
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			body, err := cli.ReadDataArg(remoteForgeData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base, body)
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: oauth-apps [create --data @f|delete <id>]")
		}
	},
}

var remoteForgeIntegrationsCmd = &cobra.Command{
	Use:   "integrations [update <id>|sync <id>|hooks <id>]",
	Short: "Board↔forge integrations",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
		if err != nil {
			return err
		}
		p := newPrinter()
		base := "/api/teams/" + team + "/forge/integrations"
		switch {
		case len(args) == 2 && args[0] == "update":
			body, err := cli.ReadDataArg(remoteForgeData)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("--data is required")
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "PATCH", base+"/"+args[1], body)
		case len(args) == 2 && args[0] == "sync":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/sync", nil)
		case len(args) == 2 && args[0] == "hooks":
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/"+args[1]+"/hooks")
		default:
			return fmt.Errorf("usage: integrations update|sync|hooks <integration-id>")
		}
	},
}

func init() {
	for _, c := range []*cobra.Command{
		remoteWebhooksListCmd, remoteWebhooksGetCmd, remoteWebhooksCreateCmd, remoteWebhooksUpdateCmd,
		remoteWebhooksDeleteCmd, remoteWebhooksRotateCmd, remoteWebhooksDeliveriesCmd,
		remoteForgeConnectionsCmd, remoteForgeRepoBotsCmd, remoteForgeOAuthAppsCmd, remoteForgeIntegrationsCmd,
	} {
		c.Flags().StringVar(&remoteTeamFlag, "team", "", "Team id (default: switched/active team)")
	}
	for _, c := range []*cobra.Command{remoteWebhooksCreateCmd, remoteWebhooksUpdateCmd} {
		c.Flags().StringVar(&remoteWebhooksData, "data", "", "Webhook config JSON (literal or @file)")
	}
	for _, c := range []*cobra.Command{remoteForgeConnectionsCmd, remoteForgeRepoBotsCmd, remoteForgeOAuthAppsCmd, remoteForgeIntegrationsCmd} {
		c.Flags().StringVar(&remoteForgeData, "data", "", "Request body JSON (literal or @file)")
	}
	remoteWebhooksCmd.AddCommand(
		remoteWebhooksListCmd, remoteWebhooksGetCmd, remoteWebhooksCreateCmd,
		remoteWebhooksUpdateCmd, remoteWebhooksDeleteCmd, remoteWebhooksRotateCmd, remoteWebhooksDeliveriesCmd,
	)
	remoteForgeCmd.AddCommand(remoteForgeConnectionsCmd, remoteForgeRepoBotsCmd, remoteForgeOAuthAppsCmd, remoteForgeIntegrationsCmd)
	remoteCmd.AddCommand(remoteWebhooksCmd, remoteForgeCmd)
}
