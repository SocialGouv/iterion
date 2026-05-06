package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var sandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Inspect and configure the iterion sandbox subsystem",
	Long: `Inspect and configure the iterion sandbox subsystem.

The sandbox provides per-run isolation (filesystem, network, env) for
coding agents. It is opt-in via the workflow's sandbox: block or the
--sandbox CLI flag — by default iterion runs without any sandbox.

Subcommands:
  doctor   Report the active driver, runtime detection, and capabilities.
`,
}

var sandboxDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Report the active sandbox driver and capabilities",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunSandboxDoctor(newPrinter())
	},
}

func init() {
	sandboxCmd.AddCommand(sandboxDoctorCmd)
	rootCmd.AddCommand(sandboxCmd)
}
