package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

func newRemoteTokensRegenerateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "regenerate <token-id>",
		Short: "Replace a token by a new one (same name, team pin and expiry; plaintext shown once) and revoke it",
		Long: "Replace a personal access token by a new one with the same name, team pin and\n" +
			"expiry instant, print the new plaintext once, then revoke the replaced token —\n" +
			"the pending invitations it emailed end with it and are named (ADR-174).\n\n" +
			"The replacement is minted first: when revoking the old token then fails, the\n" +
			"new plaintext is printed all the same and the command fails naming the token\n" +
			"still to revoke. Regenerating takes a signed-in session, and every\n" +
			"credential the CLI stores is a personal access token (a `remote login`\n" +
			"stores one too), which the instance refuses: from the CLI's own stored\n" +
			"token this command refuses before sending anything. Regenerate in the\n" +
			"studio (Account → Access tokens).",
		Args: cobra.ExactArgs(1),
		RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
			return cli.RemoteTokensRegenerate(cmd.Context(), c, p, args[0])
		}),
	}
}
