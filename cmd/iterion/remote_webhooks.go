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

var remoteWebhooksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List webhooks",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/webhooks")
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, base)
	}),
}

var remoteWebhooksGetCmd = &cobra.Command{
	Use:   "get <webhook-id>",
	Short: "Show a webhook",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/webhooks")
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/"+args[0])
	}),
}

var remoteWebhooksCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a webhook (--data @file)",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/webhooks")
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteWebhooksData, "webhook config JSON")
	}),
}

var remoteWebhooksUpdateCmd = &cobra.Command{
	Use:   "update <webhook-id>",
	Short: "Update a webhook (--data @file)",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/webhooks")
		if err != nil {
			return err
		}
		return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", base+"/"+args[0], remoteWebhooksData, "webhook config JSON")
	}),
}

var remoteWebhooksDeleteCmd = &cobra.Command{
	Use:   "delete <webhook-id>",
	Short: "Delete a webhook",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/webhooks")
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[0], nil)
	}),
}

var remoteWebhooksRotateCmd = &cobra.Command{
	Use:   "rotate <webhook-id>",
	Short: "Rotate the webhook's token",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/webhooks")
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[0]+"/rotate", nil)
	}),
}

var remoteWebhooksDeliveriesCmd = &cobra.Command{
	Use:   "deliveries <webhook-id>",
	Short: "List the webhook's recent deliveries",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/webhooks")
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/"+args[0]+"/deliveries")
	}),
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
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/forge/connections")
		if err != nil {
			return err
		}
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteForgeData, "connection JSON")
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		case args[0] == "repos" && len(args) == 2:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/"+args[1]+"/repos")
		default:
			return fmt.Errorf("usage: connections [create --data @f|delete <id>|repos <id>]")
		}
	}),
}

var remoteForgeRefreshCmd = &cobra.Command{
	Use:   "refresh <conn-id>",
	Short: "Re-probe a GitHub-App connection and re-sync its granted permissions now",
	Long: "Re-probe the installation and re-sync the stored granted permissions " +
		"immediately, so a just-changed App permission (e.g. Commit statuses: write " +
		"for the merge gate) is picked up without waiting for the periodic refresh " +
		"worker or a server restart. Prints what the installation GRANTS vs what the " +
		"minted token CARRIES (they differ, and the token is what acts).",
	Args: cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/forge/connections")
		if err != nil {
			return err
		}
		return cli.RemoteForgeRefresh(cmd.Context(), c, p, base+"/"+args[0]+"/refresh")
	}),
}

var remoteForgeRepoBotsCmd = &cobra.Command{
	Use:   "repo-bots [create|preview|delete <integration-id>]",
	Short: "Repo↔bot provisioning (forge integrations)",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/forge/repo-bots")
		if err != nil {
			return err
		}
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "preview":
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/preview")
		case args[0] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteForgeData, "request body JSON")
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: repo-bots [create --data @f|preview|delete <id>]")
		}
	}),
}

var remoteForgeOAuthAppsCmd = &cobra.Command{
	Use:   "oauth-apps [create|delete <app-id>]",
	Short: "Per-tenant forge OAuth apps",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/forge/oauth-apps")
		if err != nil {
			return err
		}
		switch {
		case len(args) == 0:
			return cli.RemoteGetPrint(cmd.Context(), c, p, base)
		case args[0] == "create":
			return cli.RemoteSendData(cmd.Context(), c, p, "POST", base, remoteForgeData, "request body JSON")
		case args[0] == "delete" && len(args) == 2:
			return cli.RemoteSendPrint(cmd.Context(), c, p, "DELETE", base+"/"+args[1], nil)
		default:
			return fmt.Errorf("usage: oauth-apps [create --data @f|delete <id>]")
		}
	}),
}

var remoteForgeIntegrationsCmd = &cobra.Command{
	Use:   "integrations [update <id>|sync <id>|hooks <id>]",
	Short: "Board↔forge integrations",
	Args:  cobra.MaximumNArgs(2),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		base, err := teamBase(cmd, c, "/forge/integrations")
		if err != nil {
			return err
		}
		switch {
		case len(args) == 2 && args[0] == "update":
			return cli.RemoteSendData(cmd.Context(), c, p, "PATCH", base+"/"+args[1], remoteForgeData, "request body JSON")
		case len(args) == 2 && args[0] == "sync":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", base+"/"+args[1]+"/sync", nil)
		case len(args) == 2 && args[0] == "hooks":
			return cli.RemoteGetPrint(cmd.Context(), c, p, base+"/"+args[1]+"/hooks")
		default:
			return fmt.Errorf("usage: integrations update|sync|hooks <integration-id>")
		}
	}),
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
	remoteForgeCmd.AddCommand(remoteForgeRefreshCmd, remoteForgeConnectionsCmd, remoteForgeRepoBotsCmd, remoteForgeOAuthAppsCmd, remoteForgeIntegrationsCmd)
	remoteCmd.AddCommand(remoteWebhooksCmd, remoteForgeCmd)
}
