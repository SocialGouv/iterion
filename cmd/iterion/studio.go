package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var studioOpts struct {
	port                   int
	bind                   string
	dir                    string
	storeDir               string
	noBrowser              bool
	noBrowserPane          bool
	maxUploadSize          int64
	maxTotalUploadSize     int64
	maxUploadsPerRun       int
	allowUploadMime        []string
	botsPaths              []string
	maxConcurrentPipelines int
}

var studioCmd = &cobra.Command{
	Use:   "studio",
	Short: "Start the iterion studio",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunStudio(cmd.Context(), cli.StudioOptions{
			Port:                   studioOpts.port,
			Bind:                   studioOpts.bind,
			Dir:                    studioOpts.dir,
			StoreDir:               studioOpts.storeDir,
			NoBrowser:              studioOpts.noBrowser,
			NoBrowserPane:          studioOpts.noBrowserPane,
			MaxUploadSize:          studioOpts.maxUploadSize,
			MaxTotalUploadSize:     studioOpts.maxTotalUploadSize,
			MaxUploadsPerRun:       studioOpts.maxUploadsPerRun,
			AllowUploadMime:        studioOpts.allowUploadMime,
			BotsPaths:              studioOpts.botsPaths,
			MaxConcurrentPipelines: studioOpts.maxConcurrentPipelines,
		}, newPrinter())
	},
}

func init() {
	f := studioCmd.Flags()
	f.IntVar(&studioOpts.port, "port", 0, "HTTP port (default: 4891)")
	// Default bind is loopback. The studio exposes unauthenticated file
	// read/write endpoints, so binding to 0.0.0.0 must be an explicit
	// operator choice — not a silent default. Use "0.0.0.0" or an
	// interface IP to expose on the LAN.
	f.StringVar(&studioOpts.bind, "bind", "127.0.0.1", "Bind address (default: 127.0.0.1; use 0.0.0.0 to expose on LAN)")
	f.StringVar(&studioOpts.dir, "dir", "", "Working directory")
	f.StringVar(&studioOpts.storeDir, "store-dir", "", "Run store directory override (default: managed store for --dir)")
	f.BoolVar(&studioOpts.noBrowser, "no-browser", false, "Don't open browser automatically")
	f.BoolVar(&studioOpts.noBrowserPane, "no-browser-pane", false, "Disable the run console's Browser pane (no preview proxy, no CDP WS, no live Chromium)")
	f.Int64Var(&studioOpts.maxUploadSize, "max-upload-size", 0, "Max bytes per attachment upload (0 = mode default: 50MB web, 1GB desktop)")
	f.Int64Var(&studioOpts.maxTotalUploadSize, "max-total-upload-size", 0, "Max cumulative bytes per run across attachments (0 = 5x max-upload-size)")
	f.IntVar(&studioOpts.maxUploadsPerRun, "max-uploads-per-run", 0, "Max distinct attachments per run (0 = 20)")
	f.StringSliceVar(&studioOpts.allowUploadMime, "allow-upload-mime", nil, "Allowed upload MIME patterns (default: image/*, application/pdf, text/*, ...)")
	f.StringSliceVar(&studioOpts.botsPaths, "bots-path", nil, "Directories or files to scan for bots (default: <dir>/bots, <dir>/examples, <dir>/.botz)")
	f.IntVar(&studioOpts.maxConcurrentPipelines, "max-concurrent-pipelines", 3, "Max root pipelines running at once (0 = unlimited). Over the cap, launches wait in the pipeline board's Todo lane and start as slots free.")
	rootCmd.AddCommand(studioCmd)
}
