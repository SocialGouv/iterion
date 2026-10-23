package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote tokens — personal access token management (/api/me/tokens).

var remoteTokensCmd = &cobra.Command{
	Use:   "tokens",
	Short: "Manage personal access tokens on the remote instance",
}

var remoteTokensListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your tokens",
	Args:  cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteTokensList(cmd.Context(), c, p)
	}),
}

var (
	remoteTokenName    string
	remoteTokenTeam    string
	remoteTokenExpires int
)

var remoteTokensCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Mint a token (--name required; plaintext shown once)",
	Long: "Mint a personal access token (--name required; the plaintext is shown once).\n\n" +
		"A token mints no token: the instance refuses it, and the CLI's stored\n" +
		"credential IS one. So this runs from a session credential only — create\n" +
		"the token in the studio (Account → Access tokens), or run it right after\n" +
		"`iterion remote login <url> --email <you>`.",
	Args: cobra.NoArgs,
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if remoteTokenName == "" {
			return fmt.Errorf("--name is required")
		}
		return cli.RemoteTokensCreate(cmd.Context(), c, p, remoteTokenName, remoteTokenTeam, remoteTokenExpires)
	}),
}

var remoteTokensRevokeCmd = &cobra.Command{
	Use:   "revoke <token-id>",
	Short: "Revoke a token",
	Args:  cobra.ExactArgs(1),
	RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteTokensRevoke(cmd.Context(), c, p, args[0])
	}),
}

func init() {
	remoteTokensCreateCmd.Flags().StringVar(&remoteTokenName, "name", "", "Token name (required)")
	remoteTokensCreateCmd.Flags().StringVar(&remoteTokenTeam, "team", "", "Pin the token to a team id")
	remoteTokensCreateCmd.Flags().IntVar(&remoteTokenExpires, "expires-days", 0, "Expiry in days (0 = platform default)")
	remoteTokensCmd.AddCommand(remoteTokensListCmd, remoteTokensCreateCmd, remoteTokensRevokeCmd)
	remoteCmd.AddCommand(remoteTokensCmd)
}
