package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var bundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Create and inspect .botz workflow bundles",
	Long: `Create and inspect .botz workflow bundles.

A .botz is a tar.gz packaging a workflow (bot.iter) with adjacent
resources: skills/, prompts/, attachments/, and an optional
manifest.yaml. See docs/bundles.md for the format reference.

Subcommands:
  init   Scaffold a new bundle source directory.
  pack   Build a deterministic .botz from a source directory.
`,
}

var bundleInitCmd = &cobra.Command{
	Use:   "init <dir>",
	Short: "Scaffold a new .botz bundle source directory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunBundleInit(args[0], newPrinter())
	},
}

var bundlePackOpts struct {
	output string
	force  bool
}

var bundlePackCmd = &cobra.Command{
	Use:   "pack <dir>",
	Short: "Build a deterministic .botz archive from a source directory",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunBundlePack(args[0], bundlePackOpts.output, bundlePackOpts.force, newPrinter())
	},
}

func init() {
	f := bundlePackCmd.Flags()
	f.StringVarP(&bundlePackOpts.output, "output", "o", "", "Output .botz path (default: <dir>.botz next to the source)")
	f.BoolVar(&bundlePackOpts.force, "force", false, "Overwrite the output file if it already exists")

	bundleCmd.AddCommand(bundleInitCmd)
	bundleCmd.AddCommand(bundlePackCmd)
	rootCmd.AddCommand(bundleCmd)
}
