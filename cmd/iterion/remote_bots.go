package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote bots / marketplace — catalog and hosted-registry management.

var remoteBotsCmd = &cobra.Command{
	Use:   "bots",
	Short: "Bot catalog on the remote instance",
}

var remoteBotsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List catalog bots",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/v1/bots")
	},
}

var remoteBotsGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show a bot's metadata + source",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/v1/bots/"+args[0])
	},
}

var remoteBotsPutData string

var remoteBotsPutCmd = &cobra.Command{
	Use:   "put <name>",
	Short: "Update a bot (--data @file)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteBotsPutData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "PUT", "/api/v1/bots/"+args[0], body)
	},
}

var remoteBotsOverlayData string

var remoteBotsOverlayCmd = &cobra.Command{
	Use:   "overlay <name>",
	Short: "Set the bot's workspace overlay (--data @file)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteBotsOverlayData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "PUT", "/api/v1/bots/"+args[0]+"/overlay", body)
	},
}

var (
	remoteBotsInstallRef  string
	remoteBotsInstallPath string
)

var remoteBotsInstallCmd = &cobra.Command{
	Use:   "install <git-url>",
	Short: "Install a bot bundle from a git URL (self-hosted instances only)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body := map[string]string{"url": args[0]}
		if remoteBotsInstallRef != "" {
			body["ref"] = remoteBotsInstallRef
		}
		if remoteBotsInstallPath != "" {
			body["path"] = remoteBotsInstallPath
		}
		p := newPrinter()
		raw, err := c.Call(cmd.Context(), "POST", "/api/v1/bots/install", body, nil)
		if err != nil {
			return err
		}
		cli.PrintRemoteJSON(p, raw)
		return nil
	},
}

var remoteBotsUploadCmd = &cobra.Command{
	Use:   "upload <file.botz>",
	Short: "Upload a packed bot bundle",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		p := newPrinter()
		raw, err := c.Upload(cmd.Context(), "/api/v1/bots/upload", "file", filepath.Base(args[0]), f, nil, nil)
		if err != nil {
			return err
		}
		cli.PrintRemoteJSON(p, raw)
		return nil
	},
}

// --- marketplace ---

var remoteMarketplaceCmd = &cobra.Command{
	Use:   "marketplace",
	Short: "Hosted bot marketplace on the remote instance",
}

var remoteMarketplaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "Browse marketplace bots",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/v1/marketplace/bots")
	},
}

var remoteMarketplaceGetCmd = &cobra.Command{
	Use:   "get <slug>",
	Short: "Show a marketplace bot",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/v1/marketplace/bots/"+args[0])
	},
}

var remoteMarketplaceOut string

var remoteMarketplaceDownloadCmd = &cobra.Command{
	Use:   "download <slug>",
	Short: "Download a bot's .botz bundle (-o output path)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		path := "/api/v1/marketplace/bots/" + args[0] + "/download"
		code, body, err := c.API(cmd.Context(), "GET", path, nil)
		if err != nil {
			return err
		}
		if code/100 != 2 {
			return fmt.Errorf("HTTP %d GET %s: %s", code, path, firstLineStr(body))
		}
		out := remoteMarketplaceOut
		if out == "" {
			out = args[0] + ".botz"
		}
		if err := os.WriteFile(out, body, 0o644); err != nil {
			return err
		}
		newPrinter().Line("Saved %s (%d bytes)", out, len(body))
		return nil
	},
}

func firstLineStr(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

var remoteMarketplaceSubmitData string

var remoteMarketplaceSubmitCmd = &cobra.Command{
	Use:   "submit",
	Short: "Submit a bundle to the marketplace (--data @file)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		body, err := cli.ReadDataArg(remoteMarketplaceSubmitData)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("--data is required")
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", "/api/v1/marketplace/submit", body)
	},
}

var remoteMarketplaceInstallCmd = &cobra.Command{
	Use:   "install <slug>",
	Short: "Install a marketplace bot into the workspace",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "POST", "/api/v1/marketplace/bots/"+args[0]+"/install", nil)
	},
}

var remoteMarketplaceUninstallCmd = &cobra.Command{
	Use:   "uninstall <slug>",
	Short: "Uninstall a marketplace bot",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, newPrinter(), "DELETE", "/api/v1/marketplace/bots/"+args[0]+"/install", nil)
	},
}

var remoteModerationReason string

var remoteMarketplaceModerationCmd = &cobra.Command{
	Use:   "moderation [approve|reject <slug>]",
	Short: "Marketplace moderation queue (cloud, moderator role)",
	Args:  cobra.MaximumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		p := newPrinter()
		if len(args) == 0 {
			return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/v1/marketplace/moderation")
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: moderation approve|reject <slug>")
		}
		switch args[0] {
		case "approve":
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/marketplace/moderation/"+args[1]+"/approve", nil)
		case "reject":
			var body []byte
			if remoteModerationReason != "" {
				body = []byte(fmt.Sprintf(`{"reason":%q}`, remoteModerationReason))
			}
			return cli.RemoteSendPrint(cmd.Context(), c, p, "POST", "/api/v1/marketplace/moderation/"+args[1]+"/reject", body)
		default:
			return fmt.Errorf("unknown moderation action %q (want approve|reject)", args[0])
		}
	},
}

func init() {
	remoteBotsPutCmd.Flags().StringVar(&remoteBotsPutData, "data", "", "Bot payload JSON (literal or @file)")
	remoteBotsOverlayCmd.Flags().StringVar(&remoteBotsOverlayData, "data", "", "Overlay JSON (literal or @file)")
	remoteBotsInstallCmd.Flags().StringVar(&remoteBotsInstallRef, "ref", "", "Git ref to install")
	remoteBotsInstallCmd.Flags().StringVar(&remoteBotsInstallPath, "path", "", "Sub-path inside the repo")
	remoteBotsCmd.AddCommand(remoteBotsListCmd, remoteBotsGetCmd, remoteBotsPutCmd, remoteBotsOverlayCmd, remoteBotsInstallCmd, remoteBotsUploadCmd)

	remoteMarketplaceDownloadCmd.Flags().StringVarP(&remoteMarketplaceOut, "output", "o", "", "Output path (default <slug>.botz)")
	remoteMarketplaceSubmitCmd.Flags().StringVar(&remoteMarketplaceSubmitData, "data", "", "Submission JSON (literal or @file)")
	remoteMarketplaceModerationCmd.Flags().StringVar(&remoteModerationReason, "reason", "", "Rejection reason")
	remoteMarketplaceCmd.AddCommand(
		remoteMarketplaceListCmd, remoteMarketplaceGetCmd, remoteMarketplaceDownloadCmd,
		remoteMarketplaceSubmitCmd, remoteMarketplaceInstallCmd, remoteMarketplaceUninstallCmd,
		remoteMarketplaceModerationCmd,
	)
	remoteCmd.AddCommand(remoteBotsCmd, remoteMarketplaceCmd)
}
