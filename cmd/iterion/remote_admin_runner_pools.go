package main

import "github.com/spf13/cobra"

func newRemoteAdminRunnerPoolsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "runner-pools", Short: "Read or replace the runner-pool registry (super-admin)"}
	cmd.AddCommand(newRemoteAdminRunnerPoolsListCmd(), newRemoteAdminRunnerPoolsSetCmd())
	return cmd
}
func init() { remoteAdminCmd.AddCommand(newRemoteAdminRunnerPoolsCmd(), newRemoteAdminTeamsCmd()) }
