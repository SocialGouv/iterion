package main

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var versionCommitOnly bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version information",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		out := cli.Version()
		if versionCommitOnly {
			out = cli.RawCommit()
		}
		fmt.Fprintln(cmd.OutOrStdout(), out)
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionCommitOnly, "commit", false, "Print only the git commit SHA")
	rootCmd.AddCommand(versionCmd)
}
