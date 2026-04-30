package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var editorOpts struct {
	port      int
	bind      string
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
			Bind:      editorOpts.bind,
			Dir:       editorOpts.dir,
			NoBrowser: editorOpts.noBrowser,
		}, newPrinter())
	},
}

func init() {
	f := editorCmd.Flags()
	f.IntVar(&editorOpts.port, "port", 0, "HTTP port (default: 4891)")
	// Default bind is loopback. The editor exposes unauthenticated file
	// read/write endpoints, so binding to 0.0.0.0 must be an explicit
	// operator choice — not a silent default. Use "0.0.0.0" or an
	// interface IP to expose on the LAN.
	f.StringVar(&editorOpts.bind, "bind", "127.0.0.1", "Bind address (default: 127.0.0.1; use 0.0.0.0 to expose on LAN)")
	f.StringVar(&editorOpts.dir, "dir", "", "Working directory")
	f.BoolVar(&editorOpts.noBrowser, "no-browser", false, "Don't open browser automatically")
	rootCmd.AddCommand(editorCmd)
}
