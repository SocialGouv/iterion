package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// remoteRunE wraps a remote command body with the shared client +
// printer prologue every `iterion remote` leaf otherwise repeats.
func remoteRunE(fn func(cmd *cobra.Command, args []string, c *cli.RemoteClient, p *cli.Printer) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		c, err := remoteClient()
		if err != nil {
			return err
		}
		return fn(cmd, args, c, newPrinter())
	}
}

// teamBase resolves the effective team and returns the team-scoped API
// base path for the given suffix (e.g. "/webhooks").
func teamBase(cmd *cobra.Command, c *cli.RemoteClient, suffix string) (string, error) {
	team, err := c.ResolveTeam(cmd.Context(), remoteTeamFlag)
	if err != nil {
		return "", err
	}
	return "/api/teams/" + team + suffix, nil
}
