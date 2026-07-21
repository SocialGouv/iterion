package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var bundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Package bot bundles into .botz archives",
	Long: `Package a bundle source directory into a distributable .botz archive.

A .botz is a tar.gz packaging a workflow (main.bot) with adjacent
resources: skills/, prompts/, attachments/, presets/, and an optional
manifest.yaml. See docs/bundles.md for the format reference.

To CREATE a bundle source directory, use ` + "`iterion bots create <slug>`" + ` —
it scaffolds the same layout plus catalog metadata, and registers the bot
so ` + "`iterion bots list`" + ` and the studio discover it.

Subcommands:
  pack   Build a deterministic .botz from a source directory.
`,
	// NoArgs makes cobra reject an unknown subcommand (notably the retired
	// `bundle init`) instead of printing help and exiting 0 — a silent
	// no-op for anyone with the old command in muscle memory.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
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

	bundleCmd.AddCommand(bundlePackCmd)
	rootCmd.AddCommand(bundleCmd)
}
