package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

func newRemoteTokensDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <token-id>",
		Short: "Suspend a token until it is enabled (its emailed invitations are refused meanwhile)",
		Args:  cobra.ExactArgs(1),
		RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
			return cli.RemoteTokensDisable(cmd.Context(), c, p, args[0])
		}),
	}
}
