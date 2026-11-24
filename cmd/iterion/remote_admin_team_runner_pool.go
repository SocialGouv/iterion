package main

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/spf13/cobra"
	"net/url"
)

func newRemoteAdminTeamRunnerPoolCmd() *cobra.Command {
	var pool string
	var clear, force bool
	cmd := &cobra.Command{Use: "runner-pool TEAM (--pool NAME | --clear)", Short: "Change a team's runner pool after draining its active runs", Args: cobra.ExactArgs(1), RunE: remoteRunE(func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error {
		if cmd.Flags().Changed("pool") == cmd.Flags().Changed("clear") {
			return fmt.Errorf("choose exactly one of --pool or --clear")
		}
		if cmd.Flags().Changed("clear") && !clear {
			return fmt.Errorf("--clear must be true")
		}
		if !clear && !queue.ValidPoolName(pool) {
			return fmt.Errorf("--pool must name a valid runner pool")
		}
		path := "/api/admin/teams/" + url.PathEscape(args[0]) + "/runner-pool"
		if force {
			path += "?force=1"
		}
		body, err := jsonBody(map[string]string{"runner_pool": pool})
		if err != nil {
			return err
		}
		return cli.RemoteSendPrint(cmd.Context(), c, p, "PUT", path, body)
	})}
	cmd.Flags().StringVar(&pool, "pool", "", "Registered pool name")
	cmd.Flags().BoolVar(&clear, "clear", false, "Return to the shared runner pool")
	cmd.Flags().BoolVar(&force, "force", false, "Explicitly bypass a known live-run cutover refusal; records the affected runs in audit")
	return cmd
}
