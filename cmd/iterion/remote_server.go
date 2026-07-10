package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remote server — instance-level information endpoints.

var remoteServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Remote instance information",
}

var remoteServerInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Instance capabilities and feature flags (GET /api/server/info)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/api/server/info")
	},
}

var remoteServerHealthCmd = &cobra.Command{
	Use:   "health",
	Short: "Instance health (GET /healthz)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return cli.RemoteGetPrint(cmd.Context(), c, newPrinter(), "/healthz")
	},
}

func init() {
	remoteServerCmd.AddCommand(remoteServerInfoCmd, remoteServerHealthCmd)
	remoteCmd.AddCommand(remoteServerCmd)
}
