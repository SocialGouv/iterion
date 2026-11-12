package main

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
	"net/url"
)

func newRemoteAdminTeamLLMFallbackCmd() *cobra.Command {
	var policy string
	var clear bool
	cmd := &cobra.Command{Use: "llm-fallback TEAM (--policy none|platform | --clear)", Short: "Set a team's credential fallback policy", Args: cobra.ExactArgs(1), RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if cmd.Flags().Changed("policy") == cmd.Flags().Changed("clear") {
			return fmt.Errorf("choose exactly one of --policy or --clear")
		}
		if cmd.Flags().Changed("clear") && !clear {
			return fmt.Errorf("--clear must be true")
		}
		if !clear && policy != "none" && policy != "platform" {
			return fmt.Errorf("--policy must be none or platform")
		}
		body, err := jsonBody(map[string]string{"llm_fallback": policy})
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", "/api/admin/teams/"+url.PathEscape(args[0])+"/llm-fallback", body)
	})}
	cmd.Flags().StringVar(&policy, "policy", "", "none (team credentials only) or platform")
	cmd.Flags().BoolVar(&clear, "clear", false, "Clear the team's explicit fallback policy")
	return cmd
}
