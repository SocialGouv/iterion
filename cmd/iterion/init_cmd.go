package main

import (
	"github.com/SocialGouv/iterion/cli"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init [dir]",
	Short: "Initialize a new project",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.InitOptions{}
		if len(args) > 0 {
			opts.Dir = args[0]
		}
		return cli.RunInit(opts, newPrinter())
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
