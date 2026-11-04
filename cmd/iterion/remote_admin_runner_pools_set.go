package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

func newRemoteAdminRunnerPoolsSetCmd() *cobra.Command {
	var data string
	cmd := &cobra.Command{Use: "set --data JSON|@file", Short: "Replace the whole runner-pool registry", Args: cobra.NoArgs, RunE: remoteRunE(func(cmd *cobra.Command, _ []string, c *cli.RemoteClient, p *cli.Printer) error {
		return cli.RemoteSendData(cmd.Context(), c, p, "PUT", "/api/admin/runner-pools", data, "runner-pool registry JSON")
	})}
	cmd.Flags().StringVar(&data, "data", "", "Whole registry JSON (literal or @file); replaces the existing registry")
	mustMarkRequired(cmd, "data")
	return cmd
}
