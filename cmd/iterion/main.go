// Command iterion is the CLI for the iterion workflow engine.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var jsonOutput bool

var rootCmd = &cobra.Command{
	Use:           "iterion",
	Short:         "Workflow orchestration engine",
	Long:          "iterion — workflow orchestration engine",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	rootCmd.Version = cli.Version()
	rootCmd.SetVersionTemplate("{{.Version}}\n")
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if jsonOutput {
			newPrinter().JSON(map[string]string{"error": err.Error()})
		} else {
			cli.PrintError(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func mustMarkRequired(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		if err := cmd.MarkFlagRequired(n); err != nil {
			panic(fmt.Sprintf("flag %q: %v", n, err))
		}
	}
}

func newPrinter() *cli.Printer {
	format := cli.OutputHuman
	if jsonOutput {
		format = cli.OutputJSON
	}
	return cli.NewPrinter(format)
}
