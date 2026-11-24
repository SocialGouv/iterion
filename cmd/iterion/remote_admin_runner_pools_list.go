package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

func newRemoteAdminRunnerPoolsListCmd() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "Read the runner-pool registry", Args: cobra.NoArgs, RunE: remoteRunE(func(cmd *cobra.Command, _ []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteGetPrint(cmd.Context(), c, p, "/api/admin/runner-pools")
	})}
}
