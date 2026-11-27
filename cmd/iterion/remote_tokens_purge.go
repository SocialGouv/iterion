package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

func newRemoteTokensPurgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "purge <token-id>",
		Short: "Remove a REVOKED token's row from the list (its audit lines stay)",
		Args:  cobra.ExactArgs(1),
		RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
			return cli.RemoteTokensPurge(cmd.Context(), c, p, args[0])
		}),
	}
}
