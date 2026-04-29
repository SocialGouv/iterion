package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var diagramOpts struct {
	view     string
	detailed bool
	full     bool
}

var diagramCmd = &cobra.Command{
	Use:   "diagram <file.iter>",
	Short: "Generate Mermaid diagram",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		view := diagramOpts.view
		switch {
		case diagramOpts.detailed:
			view = "detailed"
		case diagramOpts.full:
			view = "full"
		}
		return cli.RunDiagram(cli.DiagramOptions{
			File: args[0],
			View: view,
		}, newPrinter())
	},
}

func init() {
	f := diagramCmd.Flags()
	f.StringVar(&diagramOpts.view, "view", "", "View: compact (default), detailed, full")
	f.BoolVar(&diagramOpts.detailed, "detailed", false, "Alias for --view detailed")
	f.BoolVar(&diagramOpts.full, "full", false, "Alias for --view full")
	diagramCmd.MarkFlagsMutuallyExclusive("view", "detailed", "full")
	rootCmd.AddCommand(diagramCmd)
}
