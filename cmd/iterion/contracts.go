package main

import (
	"os"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/spf13/cobra"
)

// This probe describes this executable, never the state of a live deployment.
var contractsCmd = &cobra.Command{
	Use:   "contracts",
	Short: "Print compiled deployment contracts without network or database access",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		p := newPrinter()
		p.W = cmd.OutOrStdout()
		p.JSON(map[string]any{
			"build":  cli.Version(),
			"commit": cli.RawCommit(),
			"queue":  map[string]int{"schema_version": queue.SchemaVersion, "min_schema_version": queue.MinSchemaVersion},
			"store": map[string]any{"run_format_version": store.RunFormatVersion, "mongo_schema_version": mongostore.SchemaVersion,
				"migrations": "no compiled migration registry in pkg/store"},
			"build_skew": "unknown: server and runner builds cannot be compared from this binary alone",
		})
	},
}

// Even opt-in telemetry must not turn the local probe into network traffic.
func isContractsInvocation() bool {
	cmd, _, err := rootCmd.Find(os.Args[1:])
	return err == nil && cmd == contractsCmd
}

func init() { rootCmd.AddCommand(contractsCmd) }
