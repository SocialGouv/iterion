package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

// `iterion server` is the cloud-mode HTTP server entry point. Today
// it shares the same handler tree as `iterion editor` — the only
// behavioural deltas are:
//   - default --bind is 0.0.0.0 (cloud pods need LAN exposure to be
//     reachable behind a Service / Ingress; loopback is the wrong
//     default for a Helm-deployed pod);
//   - --no-browser is forced on (no display in a container);
//   - the command Long string spells out that this is the cloud
//     subcommand so an operator scanning `iterion --help` sees the
//     intent.
//
// When T-31 splits the cloud-side launch handler from the local
// `editor` handler tree, this command picks up the divergent code
// path; until then both subcommands route through cli.RunEditor.
//
// Cloud-ready plan §F (T-30).

var serverOpts struct {
	port     int
	bind     string
	dir      string
	storeDir string
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the cloud-mode HTTP server (editor SPA + run console + cloud API)",
	Long: `iterion server is the cloud-deployment HTTP entry point. It serves the
editor SPA, the run console (REST + WebSocket), and the launch /
resume / cancel API on a single port. Health endpoints (/healthz,
/readyz) live alongside the API.

Differences from 'iterion editor':
  - Defaults --bind to 0.0.0.0 (cloud pods are reached via Service/Ingress).
  - Forces --no-browser on (no display).
  - Reads cloud config via env (ITERION_MODE, ITERION_MONGO_URI, etc.).

For local dev, prefer 'iterion editor' which keeps the loopback bind
default and opens the browser.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunEditor(cmd.Context(), cli.EditorOptions{
			Port:      serverOpts.port,
			Bind:      serverOpts.bind,
			Dir:       serverOpts.dir,
			StoreDir:  serverOpts.storeDir,
			NoBrowser: true,
		}, newPrinter())
	},
}

func init() {
	f := serverCmd.Flags()
	f.IntVar(&serverOpts.port, "port", 4891, "HTTP port")
	f.StringVar(&serverOpts.bind, "bind", "0.0.0.0", "Bind address (default 0.0.0.0 for cloud pods)")
	f.StringVar(&serverOpts.dir, "dir", "", "Working directory")
	f.StringVar(&serverOpts.storeDir, "store-dir", "", "Run store directory (cloud mode ignores; uses Mongo+S3)")
	rootCmd.AddCommand(serverCmd)
}
