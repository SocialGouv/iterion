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
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteTokensList(cmd.Context(), c, newPrinter())
	},
}

var (
	remoteTokenName    string
	remoteTokenTeam    string
	remoteTokenExpires int
)

var remoteTokensCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Mint a token (--name required; plaintext shown once)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if remoteTokenName == "" {
			return fmt.Errorf("--name is required")
		}
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteTokensCreate(cmd.Context(), c, newPrinter(), remoteTokenName, remoteTokenTeam, remoteTokenExpires)
	},
}

var remoteTokensRevokeCmd = &cobra.Command{
	Use:   "revoke <token-id>",
	Short: "Revoke a token",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteTokensRevoke(cmd.Context(), c, newPrinter(), args[0])
	},
}

func init() {
	remoteTokensCreateCmd.Flags().StringVar(&remoteTokenName, "name", "", "Token name (required)")
	remoteTokensCreateCmd.Flags().StringVar(&remoteTokenTeam, "team", "", "Pin the token to a team id")
	remoteTokensCreateCmd.Flags().IntVar(&remoteTokenExpires, "expires-days", 0, "Expiry in days (0 = platform default)")
	remoteTokensCmd.AddCommand(remoteTokensListCmd, remoteTokensCreateCmd, remoteTokensRevokeCmd)
	remoteCmd.AddCommand(remoteTokensCmd)
}
