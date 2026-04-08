package main

import (
	"github.com/SocialGouv/iterion/cli"
	"github.com/spf13/cobra"
)

var editorOpts struct {
	port      int
	dir       string
	noBrowser bool
}

var editorCmd = &cobra.Command{
	Use:   "editor",
	Short: "Start visual workflow editor",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunEditor(cmd.Context(), cli.EditorOptions{
			Port:      editorOpts.port,
			Dir:       editorOpts.dir,
			NoBrowser: editorOpts.noBrowser,
		}, newPrinter())
	},
}

func init() {
	f := editorCmd.Flags()
	f.IntVar(&editorOpts.port, "port", 0, "HTTP port (default: 4891)")
	f.StringVar(&editorOpts.dir, "dir", "", "Working directory")
	f.BoolVar(&editorOpts.noBrowser, "no-browser", false, "Don't open browser automatically")
	rootCmd.AddCommand(editorCmd)
}
