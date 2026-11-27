package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

func newRemoteTokensEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <token-id>",
		Short: "Lift a disable (a revoked token is never enabled; takes a session: use the studio)",
		Long: "Lift a disable: the token authenticates again and the invitations it emailed\n" +
			"stand again. A revoked token is never enabled.\n\n" +
			"Enabling re-arms a credential, which takes a signed-in session; every\n" +
			"credential the CLI stores is a personal access token (a `remote login`\n" +
			"stores one too), which the instance refuses: from the CLI's own stored\n" +
			"token this command refuses before sending anything. Enable in the studio\n" +
			"(Account → Access tokens).",
		Args: cobra.ExactArgs(1),
		RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
			return cli.RemoteTokensEnable(cmd.Context(), c, p, args[0])
		}),
	}
}
