package main

import "github.com/spf13/cobra"

func newRemoteAdminTeamsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "teams", Short: "Set a team's pool or credential fallback (super-admin)"}
	cmd.AddCommand(newRemoteAdminTeamRunnerPoolCmd(), newRemoteAdminTeamLLMFallbackCmd())
	return cmd
}
