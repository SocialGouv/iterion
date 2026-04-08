package main

import (
	"github.com/SocialGouv/iterion/cli"
	"github.com/spf13/cobra"
)

var validateCmd = &cobra.Command{
	Use:   "validate <file.iter>",
	Short: "Parse, compile, and validate a workflow file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunValidate(args[0], newPrinter())
	},
}

func init() {
	rootCmd.AddCommand(validateCmd)
}
