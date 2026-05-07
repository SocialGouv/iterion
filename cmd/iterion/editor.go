package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var editorOpts struct {
	port               int
	bind               string
	dir                string
	storeDir           string
	noBrowser          bool
	maxUploadSize      int64
	maxTotalUploadSize int64
	maxUploadsPerRun   int
	allowUploadMime    []string
}

var editorCmd = &cobra.Command{
	Use:   "editor",
	Short: "Start visual workflow editor",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunEditor(cmd.Context(), cli.EditorOptions{
			Port:               editorOpts.port,
			Bind:               editorOpts.bind,
			Dir:                editorOpts.dir,
			StoreDir:           editorOpts.storeDir,
			NoBrowser:          editorOpts.noBrowser,
			MaxUploadSize:      editorOpts.maxUploadSize,
			MaxTotalUploadSize: editorOpts.maxTotalUploadSize,
			MaxUploadsPerRun:   editorOpts.maxUploadsPerRun,
			AllowUploadMime:    editorOpts.allowUploadMime,
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
	f.StringVar(&editorOpts.storeDir, "store-dir", "", "Run store directory (default: nearest .iterion ancestor of --dir, or <dir>/.iterion)")
	f.BoolVar(&editorOpts.noBrowser, "no-browser", false, "Don't open browser automatically")
	f.Int64Var(&editorOpts.maxUploadSize, "max-upload-size", 0, "Max bytes per attachment upload (0 = mode default: 50MB web, 1GB desktop)")
	f.Int64Var(&editorOpts.maxTotalUploadSize, "max-total-upload-size", 0, "Max cumulative bytes per run across attachments (0 = 5x max-upload-size)")
	f.IntVar(&editorOpts.maxUploadsPerRun, "max-uploads-per-run", 0, "Max distinct attachments per run (0 = 20)")
	f.StringSliceVar(&editorOpts.allowUploadMime, "allow-upload-mime", nil, "Allowed upload MIME patterns (default: image/*, application/pdf, text/*, ...)")
	rootCmd.AddCommand(editorCmd)
}
