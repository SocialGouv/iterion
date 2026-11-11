package main

import (
	"os"

	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/spf13/cobra"
)

// scriptHostCmd is the child half of codemode: it reads one request from
// stdin, confines itself with kernel bounds, runs the model-authored script
// and writes one response to stdout.
//
// Hidden, and spawned only by [scripthost.Host] — the convention ADR-029
// established for __claw-runner. Nothing of the multiplexed NDJSON protocol
// comes with it: a host with no tool binding has one request and one response,
// and a framing seam waits for the second consumer that would justify it.
//
// It carries no flags on purpose. Every bound travels inside the request
// document, where the child re-clamps it against the platform ceiling, so a
// crafted argv cannot widen what a tenant was granted.
var scriptHostCmd = &cobra.Command{
	Use:    scripthost.Subcommand,
	Short:  "Internal: run a codemode script in a confined, credential-less child",
	Hidden: true,
	Args:   cobra.NoArgs,
	// Cobra would otherwise print usage and its own error text on this
	// command's stdout, which is the response channel.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(*cobra.Command, []string) error {
		return scripthost.RunChild(os.Stdin, os.Stdout)
	},
}

func init() {
	rootCmd.AddCommand(scriptHostCmd)
}
